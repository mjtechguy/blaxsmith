package tooladapter

// Embedded extension stages (docs/extensions-and-runtimes.md). The worker
// checks the extension repository out at its frozen commit outside the
// workspace, copies only the template's plugins into the attempt's temporary
// home, keeps only approved hooks and MCP servers, and points Claude Code at
// the copies with --plugin-dir. Everything still runs inside the attempt
// sandbox; nothing here reaches the platform.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
)

// ExtensionMount is the frozen, approved slice of one extension version a
// stage template uses. The dispatcher derives it from the run bundle.
type ExtensionMount struct {
	ID             string        `json:"id"`
	Version        string        `json:"version"`
	RepositoryURL  string        `json:"repository_url"`
	Commit         string        `json:"commit"`
	ManifestSHA256 string        `json:"manifest_sha256"`
	Template       string        `json:"template"`
	Plugins        []PluginMount `json:"plugins"`
	// LocalMCP are approved loopback HTTP servers the extension starts in
	// the sandbox, written to an adapter-owned --mcp-config file.
	LocalMCP []LocalMCPServer   `json:"local_mcp,omitempty"`
	Signals  []extension.Signal `json:"signals,omitempty"`
}

type PluginMount struct {
	ID   string `json:"id"`
	Path string `json:"path"` // Repository-relative plugin directory.
	// MCPServers are the approved server names kept in plugin.json.
	MCPServers []string `json:"mcp_servers,omitempty"`
	// Hooks are the approved hooks kept in hooks/hooks.json.
	Hooks []PluginHook `json:"hooks,omitempty"`
}

type PluginHook struct {
	Event   string `json:"event"`
	Command string `json:"command"` // Plugin-relative script.
}

type LocalMCPServer struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

var (
	extensionID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	loopbackMCP = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost):[0-9]{2,5}/[A-Za-z0-9/_-]*$`)
	toolName    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	fieldName   = regexp.MustCompile(`^[a-z_]{1,64}$`)
)

func (m *ExtensionMount) validate(harness string) error {
	if harness != "claude-code" {
		return fmt.Errorf("%w: embedded extension stages run only on Claude Code", ErrBlocked)
	}
	if !extensionID.MatchString(m.ID) || m.Version == "" || len(m.Version) > 96 || gitfetch.Validate(m.RepositoryURL, "") != nil ||
		!gitfetch.IsCommit(m.Commit) || !sha256Hex.MatchString(m.ManifestSHA256) || len(m.Plugins) == 0 || len(m.Plugins) > 16 {
		return fmt.Errorf("%w: invalid extension mount", ErrBlocked)
	}
	seen := map[string]bool{}
	for _, p := range m.Plugins {
		if !extensionID.MatchString(p.ID) || seen[p.ID] || !extension.ValidPath(p.Path) {
			return fmt.Errorf("%w: invalid extension plugin", ErrBlocked)
		}
		seen[p.ID] = true
		for _, s := range p.MCPServers {
			if !extensionID.MatchString(s) {
				return fmt.Errorf("%w: invalid extension MCP server", ErrBlocked)
			}
		}
		for _, h := range p.Hooks {
			if !extension.ValidPath(h.Command) || !toolName.MatchString(h.Event) {
				return fmt.Errorf("%w: invalid extension hook", ErrBlocked)
			}
		}
	}
	for _, s := range m.LocalMCP {
		if !extensionID.MatchString(s.Name) || !loopbackMCP.MatchString(s.URL) {
			return fmt.Errorf("%w: extension MCP servers must be loopback inside the sandbox", ErrBlocked)
		}
	}
	for _, s := range m.Signals {
		if !seen[s.Plugin] || !extensionID.MatchString(s.Server) || !toolName.MatchString(s.Tool) ||
			!oneOf(s.Event, "phase", "handoff", "cycle", "progress") || !fieldName.MatchString(s.NameField) ||
			(s.TextField != "" && !fieldName.MatchString(s.TextField)) {
			return fmt.Errorf("%w: invalid extension signal", ErrBlocked)
		}
	}
	return nil
}

// extensionInstructions extends the bx block for embedded extension stages:
// the extension's own instructions still say AskUserQuestion, which is
// disallowed while autonomous, so each such question maps to bx ask.
const extensionInstructions = `
Embedded extension mapping (applies to the extension's commands, agents, and skills):
- Wherever those instructions say to call AskUserQuestion, run bx ask instead, one question per call: header → "title"; question → "body_md"; each option's label/description → "options" (ids "a", "b", …; mark the one the instructions recommend with "recommended": true); multiSelect → "multi_select"; always "allow_free_text": true. Use kind "interview_round" for interview rounds, "approval" for go/no-go confirmations, "escalation" for blockers, and "question" otherwise. Treat the Answer's option_ids and text exactly as the AskUserQuestion result.
- Wherever they say to wait for the user, rely on bx ask blocking; never end the session to wait.
`

// materializeExtension copies the template's plugins from the checked-out
// extension into home/extensions/<plugin>, strips unapproved hooks and MCP
// servers, and returns the plugin roots and the adapter's MCP config path
// ("" when there are no local servers).
func materializeExtension(source, home string, m *ExtensionMount) (map[string]string, string, error) {
	root, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, "", fmt.Errorf("%w: extension checkout unavailable", ErrBlocked)
	}
	roots := map[string]string{}
	for _, p := range m.Plugins {
		from := filepath.Join(root, filepath.FromSlash(p.Path))
		if resolved, err := filepath.EvalSymlinks(from); err != nil || resolved != from {
			return nil, "", fmt.Errorf("%w: extension plugin path must be a real directory", ErrBlocked)
		}
		to := filepath.Join(home, "extensions", p.ID)
		if err := copyTree(from, to); err != nil {
			return nil, "", err
		}
		if err := filterPluginConfig(to, p); err != nil {
			return nil, "", err
		}
		roots[p.ID] = to
	}
	if len(m.LocalMCP) == 0 {
		return roots, "", nil
	}
	servers := map[string]any{}
	for _, s := range m.LocalMCP {
		servers[s.Name] = map[string]string{"type": "http", "url": s.URL}
	}
	data, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return nil, "", err
	}
	config := filepath.Join(home, "extension-mcp.json")
	return roots, config, os.WriteFile(config, data, 0o400)
}

// copyTree copies regular files and directories only; a symlink anywhere in
// the plugin fails closed. Files are read-only; directories stay writable so
// an MCP server can create its environment beside its project.
func copyTree(from, to string) error {
	total := 0
	return filepath.WalkDir(from, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Name() == ".git" {
			return filepath.SkipDir
		}
		relative, err := filepath.Rel(from, name)
		if err != nil {
			return err
		}
		target := filepath.Join(to, relative)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, 0o700)
		case info.Mode().IsRegular():
			total += int(info.Size())
			if total > 64<<20 {
				return fmt.Errorf("%w: extension plugin is larger than 64 MiB", ErrBlocked)
			}
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			mode := os.FileMode(0o400)
			if info.Mode()&0o100 != 0 {
				mode = 0o500
			}
			return os.WriteFile(target, data, mode)
		default:
			return fmt.Errorf("%w: extension plugin contains a symlink or special file: %s", ErrBlocked, relative)
		}
	})
}

// filterPluginConfig rewrites the copied plugin.json and hooks/hooks.json so
// only approved MCP servers and hooks remain. Inline plugin.json hooks and a
// plugin .mcp.json are always removed; approved entries come only from the
// files the manifest validated.
func filterPluginConfig(dir string, p PluginMount) error {
	manifestPath := filepath.Join(dir, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(manifestPath)
	var manifest map[string]json.RawMessage
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		return fmt.Errorf("%w: extension plugin.json is unreadable", ErrBlocked)
	}
	delete(manifest, "hooks")
	var servers map[string]json.RawMessage
	_ = json.Unmarshal(manifest["mcpServers"], &servers)
	kept := map[string]json.RawMessage{}
	for _, name := range p.MCPServers {
		if servers[name] == nil {
			return fmt.Errorf("%w: approved MCP server %q is missing from plugin.json", ErrBlocked, name)
		}
		kept[name] = servers[name]
	}
	delete(manifest, "mcpServers")
	if len(kept) > 0 {
		manifest["mcpServers"], _ = json.Marshal(kept)
	}
	if err := rewrite(manifestPath, manifest); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(dir, ".mcp.json"))
	hooksPath := filepath.Join(dir, "hooks", "hooks.json")
	if len(p.Hooks) == 0 {
		if err := os.Remove(hooksPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err = os.ReadFile(hooksPath)
	var config struct {
		Hooks map[string][]struct {
			Matcher string            `json:"matcher,omitempty"`
			Hooks   []json.RawMessage `json:"hooks"`
		} `json:"hooks"`
	}
	if err != nil || json.Unmarshal(data, &config) != nil {
		return fmt.Errorf("%w: extension hooks.json is unreadable", ErrBlocked)
	}
	type group struct {
		Matcher string            `json:"matcher,omitempty"`
		Hooks   []json.RawMessage `json:"hooks"`
	}
	filtered := map[string][]group{}
	for event, groups := range config.Hooks {
		for _, g := range groups {
			var keep []json.RawMessage
			for _, raw := range g.Hooks {
				var h struct{ Type, Command string }
				if json.Unmarshal(raw, &h) == nil && h.Type == "command" && slices.ContainsFunc(p.Hooks, func(a PluginHook) bool {
					return a.Event == event && "${CLAUDE_PLUGIN_ROOT}/"+path.Clean(a.Command) == h.Command
				}) {
					keep = append(keep, raw)
				}
			}
			if len(keep) > 0 {
				filtered[event] = append(filtered[event], group{g.Matcher, keep})
			}
		}
	}
	if len(filtered) == 0 {
		return fmt.Errorf("%w: approved hooks are missing from hooks.json", ErrBlocked)
	}
	return rewrite(hooksPath, map[string]any{"hooks": filtered})
}

func rewrite(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	return os.WriteFile(name, data, 0o400)
}

// extensionArgs inserts the plugin and MCP flags before the trailing
// "--" prompt pair and substitutes plugin roots into the prompt.
func extensionArgs(args, resume []string, roots map[string]string, mcpConfig string, m *ExtensionMount) ([]string, []string) {
	var flags []string
	for _, p := range m.Plugins {
		flags = append(flags, "--plugin-dir", roots[p.ID])
	}
	if mcpConfig != "" {
		flags = append(flags, "--mcp-config", mcpConfig)
	}
	out := append(append(append([]string(nil), args[:len(args)-2]...), flags...), args[len(args)-2:]...)
	out[len(out)-1] = extension.ReplacePluginRoots(out[len(out)-1], roots)
	return out, append(append([]string(nil), resume...), flags...)
}

// extensionEnv points uv at the runner's pinned Python in the guild layer.
func extensionEnv() []string {
	return []string{"UV_PYTHON_INSTALL_DIR=/opt/blaxsmith/runtimes/uv/python", "UV_PYTHON_PREFERENCE=only-managed", "UV_NO_PROGRESS=1"}
}

// signalEvent turns one harness line into a bx event when it is a Claude
// stream-json tool call matching a declared signal: plugin MCP tools are
// named mcp__plugin_<plugin>_<server>__<tool>.
func signalEvents(line []byte, signals []extension.Signal) [][]byte {
	if len(signals) == 0 {
		return nil
	}
	var e struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &e) != nil || e.Type != "assistant" {
		return nil
	}
	var out [][]byte
	for _, c := range e.Message.Content {
		if c.Type != "tool_use" {
			continue
		}
		for _, s := range signals {
			if c.Name != "mcp__plugin_"+s.Plugin+"_"+s.Server+"__"+s.Tool {
				continue
			}
			name, _ := c.Input[s.NameField].(string)
			if name == "" {
				continue
			}
			event := map[string]string{"type": s.Event, "name": clipRunes(name, 120), "source": s.Plugin}
			if text, _ := c.Input[s.TextField].(string); s.TextField != "" && text != "" {
				event["text"] = clipRunes(text, 1000)
			}
			if data, err := json.Marshal(event); err == nil {
				out = append(out, data)
			}
		}
	}
	return out
}

func clipRunes(s string, n int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > n {
		return string(r[:n])
	}
	return string(r)
}
