// Package extension parses and validates extension pack manifests
// (docs/extensions-and-runtimes.md). It never executes extension content:
// validation reads JSON and checks that referenced paths exist at a commit.
package extension

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	Schema       = "blaxsmith.extension/v1alpha1"
	ManifestPath = "blaxsmith-extension.json"
	// MaxManifestBytes bounds a stored manifest.
	MaxManifestBytes = 64 << 10
	maxPromptBytes   = 8 << 10
)

type Manifest struct {
	Schema          string          `json:"$schema,omitempty"`
	SchemaVersion   string          `json:"schema_version"`
	ID              string          `json:"id"`
	Version         string          `json:"version"`
	Publisher       Publisher       `json:"publisher"`
	Description     string          `json:"description,omitempty"`
	Harnesses       []Harness       `json:"harnesses"`
	Runtimes        []Runtime       `json:"runtimes,omitempty"`
	Egress          []Egress        `json:"egress,omitempty"`
	NativeSubagents bool            `json:"native_subagents,omitempty"`
	Plugins         []Plugin        `json:"plugins,omitempty"`
	StageTemplates  []StageTemplate `json:"stage_templates"`
	Recipes         []IDPath        `json:"recipes,omitempty"`
	Skills          []PathRef       `json:"skills,omitempty"`
	Rules           []Rule          `json:"rules,omitempty"`
	Agents          []IDPath        `json:"agents,omitempty"`
	Checks          []Check         `json:"checks,omitempty"`
	Interactions    []Interaction   `json:"interactions,omitempty"`
	MCPServers      []MCPServer     `json:"mcp_servers,omitempty"`
	Hooks           []Hook          `json:"hooks,omitempty"`
	UI              *UI             `json:"ui,omitempty"`
}

type Publisher struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type Harness struct {
	Harness      string   `json:"harness"`
	MinVersion   string   `json:"min_version,omitempty"`
	Capabilities []string `json:"capabilities"`
}

type Runtime struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Egress struct {
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Purpose string `json:"purpose"`
}

type Plugin struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type StageTemplate struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Mode         string   `json:"mode"`
	Harness      string   `json:"harness"`
	Kinds        []string `json:"kinds"`
	Plugins      []string `json:"plugins,omitempty"`
	Entrypoint   string   `json:"entrypoint,omitempty"`
	Prompt       string   `json:"prompt"`
	Skills       []string `json:"skills,omitempty"`
	Rules        []string `json:"rules,omitempty"`
	Agents       []string `json:"agents,omitempty"`
	MCPServers   []string `json:"mcp_servers,omitempty"`
	Hooks        []string `json:"hooks,omitempty"`
	Interactions []string `json:"interactions,omitempty"`
	Signals      []Signal `json:"signals,omitempty"`
}

// Signal maps one MCP tool call observed in the harness stream to a bx event.
type Signal struct {
	Plugin    string `json:"plugin"`
	Server    string `json:"server"`
	Tool      string `json:"tool"`
	Event     string `json:"event"`
	NameField string `json:"name_field"`
	TextField string `json:"text_field,omitempty"`
}

type IDPath struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type PathRef struct {
	Path string `json:"path"`
}

type Rule struct {
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	AppliesTo []string `json:"applies_to,omitempty"`
}

type Check struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Verdicts []string `json:"verdicts"`
}

type Interaction struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
}

type MCPServer struct {
	ID        string `json:"id"`
	Plugin    string `json:"plugin,omitempty"`
	Server    string `json:"server,omitempty"`
	Transport string `json:"transport"`
	URL       string `json:"url,omitempty"`
	Optional  bool   `json:"optional"`
	Purpose   string `json:"purpose"`
}

type Hook struct {
	ID       string `json:"id"`
	Plugin   string `json:"plugin"`
	Event    string `json:"event"`
	Command  string `json:"command"`
	Optional bool   `json:"optional"`
	Purpose  string `json:"purpose"`
}

type UI struct {
	Phases    []UIPhase    `json:"phases,omitempty"`
	Artifacts []UIArtifact `json:"artifacts,omitempty"`
}

type UIPhase struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type UIArtifact struct {
	Glob     string `json:"glob"`
	Renderer string `json:"renderer"`
}

// Error is a validation failure at a JSON path; "$" is the whole manifest.
type Error struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Path + ": " + e.Message }

func fail(at, format string, args ...any) *Error {
	return &Error{Path: at, Message: fmt.Sprintf(format, args...)}
}

var (
	idPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	phasePattern   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	semver         = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-[0-9A-Za-z.-]{1,64})?$`)
	cliVersion     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	hostPattern    = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	toolPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	fieldPattern   = regexp.MustCompile(`^[a-z_]{1,64}$`)
	entrypoint     = regexp.MustCompile(`^/[a-z0-9-]+:[a-z0-9-]+$`)
	loopbackMCPURL = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost):[0-9]{2,5}/[A-Za-z0-9/_-]*$`)
	placeholder    = regexp.MustCompile(`\{\{plugin_root:([a-z][a-z0-9-]{0,63})\}\}`)
)

var (
	Harnesses    = []string{"claude-code", "codex", "opencode", "acp"}
	Capabilities = []string{"resume", "native_questions", "subagents", "skills", "mcp", "hooks", "plugins", "structured_protocol"}
	stageKinds   = []string{"plan", "interview", "research", "implement", "review", "verify", "integrate", "ui_review", "documentation", "architect_review"}
	hookEvents   = []string{"SessionStart", "SessionEnd", "PreToolUse", "PostToolUse", "UserPromptSubmit", "Stop", "SubagentStop", "PreCompact", "Notification"}
)

// Parse decodes one manifest strictly (no unknown fields, no duplicate
// keys, one document) and checks it. It does not look at any repository.
func Parse(data []byte) (Manifest, *Error) {
	var m Manifest
	if len(data) < 1 || len(data) > MaxManifestBytes || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
		return m, fail("$", "manifest must be 1–%d bytes of UTF-8 text without NUL", MaxManifestBytes)
	}
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return m, fail("$", "%v", err)
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&m); err != nil {
		return m, fail("$", "%s", strings.TrimPrefix(err.Error(), "json: "))
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return m, fail("$", "expected one JSON document")
	}
	if err := m.validate(); err != nil {
		return m, err
	}
	return m, nil
}

func uniqueKeys(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			if seen[key.(string)] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[key.(string)] = true
		}
		if err := uniqueKeys(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// ValidPath reports whether p is a clean repository-relative path.
func ValidPath(p string) bool {
	return p != "" && len(p) <= 512 && p != "." && !strings.HasPrefix(p, "/") && p != ".." &&
		!strings.HasPrefix(p, "../") && path.Clean(p) == p && utf8.ValidString(p) &&
		!strings.Contains(p, "\\") && !strings.ContainsFunc(p, unicode.IsControl)
}

func text(s string) bool { return strings.TrimSpace(s) != "" && len(s) <= 300 }

type ids map[string]bool

func (seen ids) add(id string) bool {
	if !idPattern.MatchString(id) || seen[id] {
		return false
	}
	seen[id] = true
	return true
}

func (m *Manifest) validate() *Error {
	if m.SchemaVersion != Schema {
		return fail("schema_version", "must be %q", Schema)
	}
	if !idPattern.MatchString(m.ID) {
		return fail("id", "must match %s", idPattern)
	}
	if !semver.MatchString(m.Version) {
		return fail("version", "must be a semantic version MAJOR.MINOR.PATCH")
	}
	if !text(m.Publisher.Name) || (m.Publisher.URL != "" && !httpsURL(m.Publisher.URL)) {
		return fail("publisher", "needs a name and an optional https URL")
	}
	if len(m.Description) > 2000 {
		return fail("description", "at most 2000 bytes")
	}
	if len(m.Harnesses) == 0 || len(m.Harnesses) > 8 {
		return fail("harnesses", "declare 1–8 supported harnesses")
	}
	harnesses := map[string]Harness{}
	for i, h := range m.Harnesses {
		at := fmt.Sprintf("harnesses[%d]", i)
		if !slices.Contains(Harnesses, h.Harness) || harnesses[h.Harness].Harness != "" {
			return fail(at+".harness", "unknown or duplicate harness %q", h.Harness)
		}
		if h.MinVersion != "" && !cliVersion.MatchString(h.MinVersion) {
			return fail(at+".min_version", "must be MAJOR.MINOR.PATCH")
		}
		seen := map[string]bool{}
		for j, c := range h.Capabilities {
			if !slices.Contains(Capabilities, c) || seen[c] {
				return fail(fmt.Sprintf("%s.capabilities[%d]", at, j), "unknown or duplicate capability %q", c)
			}
			seen[c] = true
		}
		harnesses[h.Harness] = h
	}
	runtimes := ids{}
	for i, r := range m.Runtimes {
		if !runtimes.add(r.ID) || r.Version == "" || len(r.Version) > 64 || strings.ContainsFunc(r.Version, unicode.IsSpace) {
			return fail(fmt.Sprintf("runtimes[%d]", i), "needs a unique id and a version")
		}
	}
	hosts := map[string]bool{}
	for i, e := range m.Egress {
		if !hostPattern.MatchString(e.Host) || len(e.Host) > 253 || e.Port != 443 || !text(e.Purpose) || hosts[e.Host] {
			return fail(fmt.Sprintf("egress[%d]", i), "needs a unique lowercase DNS host, port 443, and a purpose")
		}
		hosts[e.Host] = true
	}
	plugins := ids{}
	for i, p := range m.Plugins {
		if !plugins.add(p.ID) || !ValidPath(p.Path) {
			return fail(fmt.Sprintf("plugins[%d]", i), "needs a unique id and a repository-relative path")
		}
	}
	for i, list := range [][]IDPath{m.Recipes, m.Agents} {
		seen := ids{}
		name := []string{"recipes", "agents"}[i]
		for j, r := range list {
			if !seen.add(r.ID) || !ValidPath(r.Path) {
				return fail(fmt.Sprintf("%s[%d]", name, j), "needs a unique id and a repository-relative path")
			}
		}
	}
	agents := ids{}
	for _, a := range m.Agents {
		agents[a.ID] = true
	}
	for i, s := range m.Skills {
		if !ValidPath(s.Path) || path.Base(s.Path) != "SKILL.md" {
			return fail(fmt.Sprintf("skills[%d].path", i), "must name a SKILL.md")
		}
	}
	rules := ids{}
	for i, r := range m.Rules {
		if !rules.add(r.ID) || !ValidPath(r.Path) || len(r.AppliesTo) > 32 {
			return fail(fmt.Sprintf("rules[%d]", i), "needs a unique id and a repository-relative path")
		}
	}
	checks := ids{}
	for i, c := range m.Checks {
		if !checks.add(c.ID) || !text(c.Title) || len(c.Verdicts) == 0 {
			return fail(fmt.Sprintf("checks[%d]", i), "needs a unique id, a title, and verdicts")
		}
		for _, v := range c.Verdicts {
			if v != "pass" && v != "fail" && v != "blocked" {
				return fail(fmt.Sprintf("checks[%d].verdicts", i), "unknown verdict %q", v)
			}
		}
	}
	interactions := ids{}
	for i, x := range m.Interactions {
		if !interactions.add(x.ID) || !text(x.Title) ||
			!slices.Contains([]string{"question", "approval", "escalation", "interview_round"}, x.Kind) {
			return fail(fmt.Sprintf("interactions[%d]", i), "needs a unique id, a bx ask kind, and a title")
		}
	}
	servers := ids{}
	for i, s := range m.MCPServers {
		at := fmt.Sprintf("mcp_servers[%d]", i)
		if !servers.add(s.ID) || !text(s.Purpose) {
			return fail(at, "needs a unique id and a purpose")
		}
		switch {
		case s.Plugin != "":
			if !plugins[s.Plugin] || !idPattern.MatchString(s.Server) || s.URL != "" || s.Transport != "stdio" && s.Transport != "http" {
				return fail(at, "a plugin server names a declared plugin and its server id, without a url")
			}
		case s.Transport == "http":
			if s.Server != "" || !loopbackMCPURL.MatchString(s.URL) {
				return fail(at+".url", "a sandbox-started server must use a loopback http URL")
			}
		default:
			return fail(at, "must be a plugin server or a loopback http server started in the sandbox")
		}
	}
	hooks := ids{}
	for i, h := range m.Hooks {
		at := fmt.Sprintf("hooks[%d]", i)
		if !hooks.add(h.ID) || !plugins[h.Plugin] || !slices.Contains(hookEvents, h.Event) || !ValidPath(h.Command) || !text(h.Purpose) {
			return fail(at, "needs a unique id, a declared plugin, a known event, a plugin-relative command, and a purpose")
		}
	}
	if len(m.StageTemplates) == 0 || len(m.StageTemplates) > 32 {
		return fail("stage_templates", "declare 1–32 stage templates")
	}
	templates := ids{}
	for i, t := range m.StageTemplates {
		at := fmt.Sprintf("stage_templates[%d]", i)
		if !templates.add(t.ID) || !text(t.Title) {
			return fail(at+".id", "needs a unique id and a title")
		}
		if t.Mode != "embedded" && t.Mode != "decomposed" {
			return fail(at+".mode", "must be embedded or decomposed")
		}
		h, ok := harnesses[t.Harness]
		if !ok {
			return fail(at+".harness", "harness %q is not declared in harnesses", t.Harness)
		}
		if len(t.Kinds) == 0 {
			return fail(at+".kinds", "declare the stage kinds this template fills")
		}
		for j, k := range t.Kinds {
			if !slices.Contains(stageKinds, k) || slices.Index(t.Kinds, k) != j {
				return fail(fmt.Sprintf("%s.kinds[%d]", at, j), "unknown or duplicate stage kind %q", k)
			}
		}
		if t.Prompt == "" || len(t.Prompt) > maxPromptBytes || strings.ContainsRune(t.Prompt, 0) {
			return fail(at+".prompt", "needs 1–%d bytes", maxPromptBytes)
		}
		if t.Entrypoint != "" && !entrypoint.MatchString(t.Entrypoint) {
			return fail(at+".entrypoint", "must look like /plugin:command")
		}
		for _, ref := range []struct {
			field string
			list  []string
			known ids
		}{{"plugins", t.Plugins, plugins}, {"rules", t.Rules, rules}, {"agents", t.Agents, agents},
			{"mcp_servers", t.MCPServers, servers}, {"hooks", t.Hooks, hooks}, {"interactions", t.Interactions, interactions}} {
			for j, id := range ref.list {
				if !ref.known[id] || slices.Index(ref.list, id) != j {
					return fail(fmt.Sprintf("%s.%s[%d]", at, ref.field, j), "%q is not declared or is repeated", id)
				}
			}
		}
		for j, s := range t.Skills {
			if !ValidPath(s) || path.Base(s) != "SKILL.md" {
				return fail(fmt.Sprintf("%s.skills[%d]", at, j), "must name a SKILL.md")
			}
		}
		for _, match := range placeholder.FindAllStringSubmatch(t.Prompt, -1) {
			if !slices.Contains(t.Plugins, match[1]) {
				return fail(at+".prompt", "placeholder names plugin %q that the template does not use", match[1])
			}
		}
		if t.Mode == "embedded" && len(t.Plugins) > 0 && !slices.Contains(h.Capabilities, "plugins") {
			return fail(at+".plugins", "plugins need a harness with the plugins capability")
		}
		for _, id := range t.MCPServers {
			if s := server(m.MCPServers, id); s.Plugin != "" && !slices.Contains(t.Plugins, s.Plugin) {
				return fail(at+".mcp_servers", "server %q belongs to plugin %q, which the template does not use", id, s.Plugin)
			}
		}
		for _, id := range t.Hooks {
			for _, hook := range m.Hooks {
				if hook.ID == id && !slices.Contains(t.Plugins, hook.Plugin) {
					return fail(at+".hooks", "hook %q belongs to plugin %q, which the template does not use", id, hook.Plugin)
				}
			}
		}
		for j, s := range t.Signals {
			sat := fmt.Sprintf("%s.signals[%d]", at, j)
			if !slices.Contains(t.Plugins, s.Plugin) || !idPattern.MatchString(s.Server) || !toolPattern.MatchString(s.Tool) ||
				!slices.Contains([]string{"phase", "handoff", "cycle", "progress"}, s.Event) ||
				!fieldPattern.MatchString(s.NameField) || (s.TextField != "" && !fieldPattern.MatchString(s.TextField)) {
				return fail(sat, "needs a template plugin, a server, a tool name, a phase/handoff/cycle/progress event, and field names")
			}
		}
	}
	if m.UI != nil {
		seen := map[string]bool{}
		for i, p := range m.UI.Phases {
			if !phasePattern.MatchString(p.ID) || seen[p.ID] || !text(p.Title) {
				return fail(fmt.Sprintf("ui.phases[%d]", i), "needs a unique id and a title")
			}
			seen[p.ID] = true
		}
		for i, a := range m.UI.Artifacts {
			if a.Glob == "" || len(a.Glob) > 256 || !slices.Contains([]string{"markdown", "json", "table", "image", "text"}, a.Renderer) {
				return fail(fmt.Sprintf("ui.artifacts[%d]", i), "needs a glob and a known renderer")
			}
		}
	}
	return nil
}

func server(list []MCPServer, id string) MCPServer {
	for _, s := range list {
		if s.ID == id {
			return s
		}
	}
	return MCPServer{}
}

func httpsURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && u.Scheme == "https" && u.Host != "" && u.User == nil
}

// Template returns the named stage template.
func (m Manifest) Template(id string) (StageTemplate, bool) {
	for _, t := range m.StageTemplates {
		if t.ID == id {
			return t, true
		}
	}
	return StageTemplate{}, false
}

// PluginPath returns the repository path of a declared plugin.
func (m Manifest) PluginPath(id string) string {
	for _, p := range m.Plugins {
		if p.ID == id {
			return p.Path
		}
	}
	return ""
}

// Permission is one thing an administrator approves at install.
type Permission struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // subagents, mcp, hook, egress, runtime
	Description string `json:"description"`
	Optional    bool   `json:"optional"`
}

// Permissions derives the declared permissions, in a stable order.
func (m Manifest) Permissions() []Permission {
	var out []Permission
	if m.NativeSubagents {
		out = append(out, Permission{"subagents", "subagents", "Embedded templates may use the harness's own subagents and teams inside their one sandbox", false})
	}
	for _, s := range m.MCPServers {
		where := "started inside the sandbox at " + s.URL
		if s.Plugin != "" {
			where = "server " + s.Server + " from plugin " + s.Plugin + ", run inside the sandbox"
		}
		out = append(out, Permission{"mcp:" + s.ID, "mcp", "MCP server " + s.ID + " (" + where + "): " + s.Purpose, s.Optional})
	}
	for _, h := range m.Hooks {
		out = append(out, Permission{"hook:" + h.ID, "hook", h.Event + " hook " + h.Plugin + "/" + h.Command + " (sandbox only): " + h.Purpose, h.Optional})
	}
	for _, e := range m.Egress {
		out = append(out, Permission{"egress:" + e.Host, "egress", fmt.Sprintf("Sandbox egress to %s:%d: %s", e.Host, e.Port, e.Purpose), false})
	}
	for _, r := range m.Runtimes {
		out = append(out, Permission{"runtime:" + r.ID, "runtime", "Runner runtime layer " + r.ID + " " + r.Version, false})
	}
	return out
}

// ErrApproval means the approved set omits a required permission or names
// one the manifest does not declare.
var ErrApproval = errors.New("approved permissions do not match the manifest")

// Approve checks an administrator's approval and returns it sorted.
func (m Manifest) Approve(approved []string) ([]string, error) {
	declared := map[string]Permission{}
	for _, p := range m.Permissions() {
		declared[p.ID] = p
	}
	out := slices.Clone(approved)
	slices.Sort(out)
	for i, id := range out {
		if _, ok := declared[id]; !ok || (i > 0 && out[i-1] == id) {
			return nil, fmt.Errorf("%w: %q", ErrApproval, id)
		}
	}
	for id, p := range declared {
		if !p.Optional && !slices.Contains(out, id) {
			return nil, fmt.Errorf("%w: required permission %q is not approved", ErrApproval, id)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out, nil
}

// Digest is the lowercase hex SHA-256 of data.
func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// PermissionsDigest hashes a sorted approval list.
func PermissionsDigest(approved []string) string {
	data, _ := json.Marshal(approved)
	return Digest(data)
}

// ReplacePluginRoots substitutes {{plugin_root:<id>}} with the materialized
// directory of each plugin; unknown ids are left for validation to reject.
func ReplacePluginRoots(prompt string, roots map[string]string) string {
	return placeholder.ReplaceAllStringFunc(prompt, func(match string) string {
		id := placeholder.FindStringSubmatch(match)[1]
		if root, ok := roots[id]; ok {
			return root
		}
		return match
	})
}

// Pin is one installed, approved extension version resolved for a run.
type Pin struct {
	ID            string
	Version       string
	RepositoryURL string
	Commit        string
	Manifest      []byte
	Approved      []string
}

// Frozen is the extension record a run bundle carries. Manifest keeps the
// exact installed bytes (JSON base64), so its digest survives storage.
type Frozen struct {
	ID                string   `json:"id"`
	Version           string   `json:"version"`
	RepositoryURL     string   `json:"repository_url"`
	Commit            string   `json:"commit"`
	ManifestSHA256    string   `json:"manifest_sha256"`
	Approved          []string `json:"approved_permissions"`
	PermissionsSHA256 string   `json:"permissions_sha256"`
	Manifest          []byte   `json:"manifest"`
}

// Freeze validates a pin and returns its bundle record and parsed manifest.
func (p Pin) Freeze() (Frozen, Manifest, error) {
	m, perr := Parse(p.Manifest)
	if perr != nil {
		return Frozen{}, m, perr
	}
	if m.ID != p.ID || m.Version != p.Version {
		return Frozen{}, m, fmt.Errorf("extension %s@%s manifest names %s@%s", p.ID, p.Version, m.ID, m.Version)
	}
	approved, err := m.Approve(p.Approved)
	if err != nil {
		return Frozen{}, m, err
	}
	return Frozen{ID: p.ID, Version: p.Version, RepositoryURL: p.RepositoryURL, Commit: p.Commit,
		ManifestSHA256: Digest(p.Manifest), Approved: approved, PermissionsSHA256: PermissionsDigest(approved),
		Manifest: slices.Clone(p.Manifest)}, m, nil
}

// Load re-parses a frozen record after checking its digests.
func (f Frozen) Load() (Manifest, error) {
	if Digest(f.Manifest) != f.ManifestSHA256 || PermissionsDigest(f.Approved) != f.PermissionsSHA256 {
		return Manifest{}, errors.New("frozen extension digest mismatch")
	}
	m, perr := Parse(f.Manifest)
	if perr != nil {
		return m, perr
	}
	return m, nil
}

// Approves reports whether permission id was approved.
func (f Frozen) Approves(id string) bool { return slices.Contains(f.Approved, id) }

var templateRef = regexp.MustCompile(`^([a-z][a-z0-9-]{0,63})@((?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})\.(?:0|[1-9][0-9]{0,8})(?:-[0-9A-Za-z.-]{1,64})?)/([a-z][a-z0-9-]{0,63})$`)

// ParseTemplateRef splits "extension@version/template".
func ParseTemplateRef(ref string) (id, version, template string, ok bool) {
	m := templateRef.FindStringSubmatch(ref)
	if m == nil {
		return "", "", "", false
	}
	return m[1], m[2], m[3], true
}
