package tooladapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"gopkg.in/yaml.v3"
)

const (
	WorkerBinary   = "/usr/local/bin/blaxsmith-tool-worker"
	CredentialFile = "/run/blaxsmith/agent-credential.json"
	maxTaskArg     = 120 << 10 // AX passes the JSON as one Linux argv element.
)

// Request is public task configuration, bound to the immutable AX task. It
// deliberately has no credential, connection ID, or lease token.
type Request struct {
	AttemptID       string           `json:"attempt_id"`
	RepositoryURL   string           `json:"repository_url"`
	SourceRef       string           `json:"source_ref"`
	SourceCommit    string           `json:"source_commit"`
	SourceDirectory string           `json:"source_directory,omitempty"`
	Runtime         Runtime          `json:"runtime"`
	Profile         recipe.Profile   `json:"profile"`
	Prompt          string           `json:"prompt"`
	FrozenArtifacts []ArtifactDigest `json:"frozen_artifacts,omitempty"`
	TimeoutSeconds  int              `json:"timeout_seconds"` // idle timeout
	// MaxRuntimeSeconds caps total stage time; 0 is unlimited.
	MaxRuntimeSeconds int `json:"max_runtime_seconds,omitempty"`
	MaxOutputBytes    int `json:"max_output_bytes"`
	// Extension is set for an embedded extension stage template.
	Extension *ExtensionMount `json:"extension,omitempty"`
	// Gateway is set for brokered_gateway attempts (gateway.go).
	Gateway *Gateway `json:"gateway,omitempty"`
}

// ArtifactDigest binds prompt context to regular files in the pinned checkout.
// Only public Git-authored text belongs here; no credentials or live config.
type ArtifactDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Command prepares a task argv without a shell or a secret. The AX bridge
// must also pin Runtime.Image to the observed actor image.
func Command(request Request) ([]string, error) {
	if request.AttemptID == "" || len(request.AttemptID) > 128 || strings.ContainsAny(request.AttemptID, " \t\r\n\x00") {
		return nil, ErrBlocked
	}
	if err := gitfetch.Validate(request.RepositoryURL, request.SourceRef); err != nil || !gitfetch.IsCommit(request.SourceCommit) ||
		request.SourceDirectory != "" && (request.SourceDirectory == "." || request.SourceDirectory == ".." ||
			strings.ContainsAny(request.SourceDirectory, "/\\\r\n\x00")) {
		return nil, fmt.Errorf("%w: invalid frozen public Git source", ErrBlocked)
	}
	profile, err := frozenProfile(request)
	if err != nil {
		return nil, err
	}
	if _, err := Prepare(request.Runtime, profile, request.Prompt,
		time.Duration(request.TimeoutSeconds)*time.Second, request.MaxOutputBytes); err != nil {
		return nil, err
	}
	if request.MaxRuntimeSeconds < 0 || request.MaxRuntimeSeconds > recipe.MaxRuntimeCap {
		return nil, fmt.Errorf("%w: invalid max runtime", ErrBlocked)
	}
	if _, err := credentialProvider(request.Profile); err != nil {
		return nil, err
	}
	if request.Extension != nil {
		if err := request.Extension.validate(request.Profile.Harness); err != nil {
			return nil, err
		}
	}
	if err := request.Gateway.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > maxTaskArg {
		return nil, ErrBlocked
	}
	return []string{WorkerBinary, string(body)}, nil
}

// credential is the runner's 0600 model file: exactly one of an API key or
// (Codex, provider openai) a ChatGPT sign-in auth.json.
type credential struct {
	AttemptID     string `json:"attempt_id"`
	Provider      string `json:"provider"`
	ExpiresAt     int64  `json:"expires_at"`
	APIKey        string `json:"api_key,omitempty"`
	CodexAuthJSON string `json:"codex_auth_json,omitempty"`
}

// Execute is the AX task command's pod-side entrypoint. A missing, stale, or
// mismatched bootstrap credential prevents the CLI from starting.
func Execute(ctx context.Context, encoded, workdir, credentialPath string) ([]byte, error) {
	return execute(ctx, encoded, workdir, credentialPath, gitfetch.Checkout)
}

func execute(ctx context.Context, encoded, workdir, credentialPath string,
	checkout func(context.Context, string, string, string, string) error) ([]byte, error) {
	if len(encoded) == 0 || len(encoded) > maxTaskArg {
		return nil, ErrBlocked
	}
	var request Request
	if err := strictJSON([]byte(encoded), &request); err != nil {
		return nil, ErrBlocked
	}
	if _, err := Command(request); err != nil {
		return nil, err
	}
	provider, err := credentialProvider(request.Profile)
	if err != nil {
		return nil, err
	}
	key, codexAuth, expiry, err := readCredential(credentialPath, request.AttemptID, provider, request.Profile.Harness)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	defer clear(codexAuth)
	sourcePath := workdir
	if request.SourceDirectory == "" {
		if checkout == nil || checkout(ctx, request.RepositoryURL, request.SourceRef, request.SourceCommit, sourcePath) != nil {
			return nil, fmt.Errorf("%w: pinned source checkout failed", ErrBlocked)
		}
	} else {
		var err error
		sourcePath, err = workspaceSourcePath(workdir, request.SourceDirectory)
		if err != nil || verifyWorkspaceCheckout(ctx, sourcePath, request.RepositoryURL, request.SourceCommit) != nil {
			return nil, fmt.Errorf("%w: AX workspace source does not match the frozen input", ErrBlocked)
		}
	}
	if err := verifyFrozenArtifacts(sourcePath, request.FrozenArtifacts); err != nil {
		return nil, err
	}
	profile, err := frozenProfile(request)
	if err != nil {
		return nil, err
	}
	in, err := Prepare(request.Runtime, profile, request.Prompt,
		time.Duration(request.TimeoutSeconds)*time.Second, request.MaxOutputBytes)
	if err != nil {
		return nil, err
	}
	in.skillArtifacts = selectedSkillArtifacts(profile.Skills, request.FrozenArtifacts)
	if request.Extension != nil {
		// The extension checkout lives outside the workspace so it is never
		// ambient project configuration; only its copied plugins are used.
		source, err := os.MkdirTemp("", "blaxsmith-ext-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(source)
		ext := request.Extension
		if checkout == nil || checkout(ctx, ext.RepositoryURL, ext.Commit, ext.Commit, source) != nil {
			return nil, fmt.Errorf("%w: pinned extension checkout failed", ErrBlocked)
		}
		in = in.withExtension(ext, source)
	}
	in.base = request.SourceCommit
	in.maxRuntime = time.Duration(request.MaxRuntimeSeconds) * time.Second
	leaseContext, cancel := watchLease(ctx, expiry)
	defer cancel()
	if codexAuth != nil {
		secrets, err := ParseCodexAuth(codexAuth)
		if err != nil {
			return nil, err
		}
		output, err := Run(leaseContext, in.withCodexAuth(codexAuth), sourcePath, nil)
		return redactSecrets(output, secrets), err
	}
	env := []string{nativeCredentialEnv(request.Profile.Harness, provider) + "=" + string(key)}
	if request.Gateway != nil {
		env, in.gatewayBaseURL = gatewayCredentialEnv(request.Gateway, request.Profile.Harness, provider, key), request.Gateway.BaseURL
	}
	output, err := Run(leaseContext, in, sourcePath, env)
	return bytes.ReplaceAll(output, key, []byte("[redacted]")), err
}

func workspaceSourcePath(root, name string) (string, error) {
	if !filepath.IsAbs(root) || name == "" || name == "." || name == ".." ||
		strings.ContainsAny(name, "/\\\r\n\x00") {
		return "", ErrBlocked
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", ErrBlocked
	}
	source := filepath.Join(root, name)
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", ErrBlocked
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil || filepath.Dir(resolved) != root {
		return "", ErrBlocked
	}
	return resolved, nil
}

func verifyWorkspaceCheckout(ctx context.Context, workdir, repositoryURL, commit string) error {
	if gitfetch.Validate(repositoryURL, "") != nil || !gitfetch.IsCommit(commit) {
		return ErrBlocked
	}
	gitDir := filepath.Join(workdir, ".git")
	info, err := os.Lstat(gitDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrBlocked
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https",
		"GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1", "GIT_OPTIONAL_LOCKS=0"}
	run := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workdir}, args...)...)
		cmd.Env = env
		out, err := cmd.Output()
		return strings.TrimSpace(string(out)), err
	}
	root, err := run("rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(root) != filepath.Clean(workdir) {
		return ErrBlocked
	}
	origin, err := run("config", "--local", "--get", "remote.origin.url")
	if err != nil || origin != repositoryURL {
		return ErrBlocked
	}
	got, err := run("rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || got != commit {
		return ErrBlocked
	}
	return nil
}

func frozenProfile(request Request) (recipe.Profile, error) {
	seen := make(map[string]bool, len(request.FrozenArtifacts))
	for _, artifact := range request.FrozenArtifacts {
		if !filepath.IsLocal(artifact.Path) || path.Clean(artifact.Path) != artifact.Path ||
			strings.ContainsAny(artifact.Path, "\\\r\n\x00") || !sha256Hex.MatchString(artifact.SHA256) || seen[artifact.Path] ||
			len(seen) >= 256 {
			return recipe.Profile{}, fmt.Errorf("%w: invalid frozen artifact manifest", ErrBlocked)
		}
		seen[artifact.Path] = true
	}
	for _, name := range append(append([]string(nil), request.Profile.Instructions...), request.Profile.Skills...) {
		if !seen[name] {
			return recipe.Profile{}, fmt.Errorf("%w: instruction or skill is not frozen", ErrBlocked)
		}
	}
	if _, err := skillFilesByRoot(request.Profile.Skills); err != nil {
		return recipe.Profile{}, err
	}
	profile := request.Profile
	profile.Instructions = nil
	return profile, nil
}

func verifyFrozenArtifacts(workdir string, artifacts []ArtifactDigest) error {
	if len(artifacts) > 256 {
		return fmt.Errorf("%w: too many frozen artifacts", ErrBlocked)
	}
	total := 0
	allowedAgents := map[string]bool{}
	for _, artifact := range artifacts {
		data, err := readFrozenArtifact(workdir, artifact)
		if err != nil {
			return err
		}
		total += len(data)
		if total > 16<<20 {
			return fmt.Errorf("%w: frozen artifact bundle is too large", ErrBlocked)
		}
		if path.Base(artifact.Path) == "AGENTS.md" {
			allowedAgents[artifact.Path] = true
		}
	}
	return filepath.WalkDir(workdir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		base := entry.Name()
		if base == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if base == ".codex" || base == ".opencode" || base == ".claude" || base == ".agents" ||
			base == ".mcp.json" || base == "opencode.json" || base == "opencode.jsonc" || base == "CLAUDE.md" || base == "CLAUDE.local.md" {
			return fmt.Errorf("%w: ambient project CLI configuration", ErrBlocked)
		}
		if base == "AGENTS.md" {
			relative, err := filepath.Rel(workdir, name)
			if err != nil || !allowedAgents[filepath.ToSlash(relative)] || !entry.Type().IsRegular() {
				return fmt.Errorf("%w: unlisted project instructions", ErrBlocked)
			}
		}
		return nil
	})
}

var skillName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// skillFilesByRoot accepts portable Agent Skills: each selected directory has
// one SKILL.md, and any other selected files must be declared beneath it.
func skillFilesByRoot(files []string) (map[string][]string, error) {
	roots := map[string][]string{}
	seen := map[string]bool{}
	for _, file := range files {
		if !filepath.IsLocal(file) || path.Clean(file) != file || strings.ContainsAny(file, "\\\r\n\x00") || seen[file] {
			return nil, fmt.Errorf("%w: invalid or duplicate skill artifact path", ErrBlocked)
		}
		seen[file] = true
		if path.Base(file) != "SKILL.md" {
			continue
		}
		root := path.Dir(file)
		name := path.Base(root)
		if root == "." || len(name) > 64 || !skillName.MatchString(name) {
			return nil, fmt.Errorf("%w: skill directory must have a portable name", ErrBlocked)
		}
		roots[root] = append(roots[root], file)
	}
	for root, manifests := range roots {
		if len(manifests) != 1 {
			return nil, fmt.Errorf("%w: skill directory has duplicate manifests", ErrBlocked)
		}
		for other := range roots {
			if root != other && (strings.HasPrefix(root, other+"/") || strings.HasPrefix(other, root+"/")) {
				return nil, fmt.Errorf("%w: nested selected skills are ambiguous", ErrBlocked)
			}
		}
	}
	for _, file := range files {
		var root string
		manifest := false
		for candidate := range roots {
			if file == roots[candidate][0] || strings.HasPrefix(file, candidate+"/") {
				if root != "" {
					return nil, fmt.Errorf("%w: skill file belongs to multiple skills", ErrBlocked)
				}
				root = candidate
				manifest = file == roots[candidate][0]
			}
		}
		if root == "" {
			return nil, fmt.Errorf("%w: every skill file must be under a selected SKILL.md", ErrBlocked)
		}
		if !manifest {
			roots[root] = append(roots[root], file)
		}
	}
	return roots, nil
}

func selectedSkillArtifacts(skills []string, artifacts []ArtifactDigest) []ArtifactDigest {
	selected := make(map[string]bool, len(skills))
	for _, file := range skills {
		selected[file] = true
	}
	out := make([]ArtifactDigest, 0, len(skills))
	for _, artifact := range artifacts {
		if selected[artifact.Path] {
			out = append(out, artifact)
		}
	}
	return out
}

func materializeSkills(workdir, home, harness string, skills []string, artifacts []ArtifactDigest) (string, error) {
	roots, err := skillFilesByRoot(skills)
	if err != nil {
		return "", err
	}
	manifest := make(map[string]ArtifactDigest, len(artifacts))
	for _, artifact := range artifacts {
		manifest[artifact.Path] = artifact
	}
	base := ""
	claudeRoot := ""
	switch harness {
	case "codex":
		base = filepath.Join(home, ".agents", "skills")
	case "claude-code":
		claudeRoot = filepath.Join(home, "skill-source")
		base = filepath.Join(claudeRoot, ".claude", "skills")
	case "opencode":
		base = filepath.Join(home, ".config", "opencode", "skills")
	default:
		return "", fmt.Errorf("%w: native skills are unsupported for this harness", ErrBlocked)
	}
	if len(skills) == 0 {
		return "", nil
	}
	if len(manifest) != len(skills) {
		return "", fmt.Errorf("%w: selected skills are not bound to frozen artifacts", ErrBlocked)
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return "", err
	}
	names := map[string]bool{}
	orderedRoots := make([]string, 0, len(roots))
	for root := range roots {
		orderedRoots = append(orderedRoots, root)
	}
	sort.Strings(orderedRoots)
	for _, root := range orderedRoots {
		name := path.Base(root)
		if names[name] {
			return "", fmt.Errorf("%w: selected skill names must be unique", ErrBlocked)
		}
		names[name] = true
		files := roots[root]
		sort.Strings(files)
		var metadata skillFrontmatter
		for _, file := range files {
			artifact, ok := manifest[file]
			if !ok {
				return "", fmt.Errorf("%w: skill content is not digest-bound", ErrBlocked)
			}
			data, err := readFrozenArtifact(workdir, artifact)
			if err != nil {
				return "", err
			}
			if file == root+"/SKILL.md" {
				metadata, err = parseSkillFrontmatter(data)
				if err != nil || metadata.Name != name {
					return "", fmt.Errorf("%w: skill frontmatter must use its portable directory name and description", ErrBlocked)
				}
			}
			relative := strings.TrimPrefix(file, root+"/")
			destination := filepath.Join(base, name, filepath.FromSlash(relative))
			if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
				return "", err
			}
			if err := os.WriteFile(destination, data, 0400); err != nil {
				return "", err
			}
		}
		if metadata.Name == "" || metadata.Description == "" {
			return "", fmt.Errorf("%w: skill manifest is missing portable metadata", ErrBlocked)
		}
	}
	if harness == "claude-code" {
		return claudeRoot, nil
	}
	return "", nil
}

type skillFrontmatter struct{ Name, Description string }

func parseSkillFrontmatter(data []byte) (skillFrontmatter, error) {
	var result skillFrontmatter
	lines := strings.Split(string(data), "\n")
	if len(lines) < 4 || strings.TrimSuffix(lines[0], "\r") != "---" {
		return result, fmt.Errorf("%w: missing skill frontmatter", ErrBlocked)
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSuffix(lines[i], "\r") == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return result, fmt.Errorf("%w: unterminated skill frontmatter", ErrBlocked)
	}
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &document); err != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return result, fmt.Errorf("%w: invalid skill frontmatter", ErrBlocked)
	}
	seen := map[string]bool{}
	for i := 0; i < len(document.Content[0].Content); i += 2 {
		key, value := document.Content[0].Content[i], document.Content[0].Content[i+1]
		if key.Kind != yaml.ScalarNode || seen[key.Value] {
			return result, fmt.Errorf("%w: invalid or duplicate skill frontmatter field", ErrBlocked)
		}
		seen[key.Value] = true
		if key.Value == "metadata" {
			if value.Kind != yaml.MappingNode || len(value.Content)%2 != 0 {
				return result, fmt.Errorf("%w: invalid skill metadata", ErrBlocked)
			}
			metadataSeen := map[string]bool{}
			for j := 0; j < len(value.Content); j += 2 {
				k, v := value.Content[j], value.Content[j+1]
				if k.Kind != yaml.ScalarNode || v.Kind != yaml.ScalarNode || v.Tag != "!!str" || metadataSeen[k.Value] {
					return result, fmt.Errorf("%w: skill metadata must contain unique string values", ErrBlocked)
				}
				metadataSeen[k.Value] = true
			}
			continue
		}
		if value.Kind != yaml.ScalarNode || value.Tag != "!!str" {
			return result, fmt.Errorf("%w: skill frontmatter supports only portable scalar metadata", ErrBlocked)
		}
		switch key.Value {
		case "name":
			result.Name = value.Value
		case "description":
			result.Description = strings.TrimSpace(value.Value)
		case "license", "compatibility":
		default:
			return result, fmt.Errorf("%w: harness-specific skill frontmatter is not supported", ErrBlocked)
		}
	}
	if len(result.Name) > 64 || !skillName.MatchString(result.Name) || len(result.Description) == 0 || len(result.Description) > 1024 {
		return result, fmt.Errorf("%w: invalid portable skill name or description", ErrBlocked)
	}
	return result, nil
}

func readFrozenArtifact(workdir string, artifact ArtifactDigest) ([]byte, error) {
	if !filepath.IsLocal(artifact.Path) || path.Clean(artifact.Path) != artifact.Path ||
		strings.ContainsAny(artifact.Path, "\\\r\n\x00") || !sha256Hex.MatchString(artifact.SHA256) {
		return nil, fmt.Errorf("%w: invalid frozen artifact path", ErrBlocked)
	}
	root, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return nil, fmt.Errorf("%w: frozen artifact unavailable", ErrBlocked)
	}
	parts := strings.Split(filepath.ToSlash(artifact.Path), "/")
	name := root
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("%w: invalid frozen artifact path", ErrBlocked)
		}
		name = filepath.Join(name, filepath.FromSlash(part))
		info, err := os.Lstat(name)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (i < len(parts)-1 && !info.IsDir()) {
			return nil, fmt.Errorf("%w: frozen artifact unavailable", ErrBlocked)
		}
		if i == len(parts)-1 && (!info.Mode().IsRegular() || info.Size() > 16<<20) {
			return nil, fmt.Errorf("%w: frozen artifact unavailable", ErrBlocked)
		}
	}
	data, err := os.ReadFile(name)
	if err != nil || len(data) > 16<<20 {
		return nil, fmt.Errorf("%w: frozen artifact unavailable", ErrBlocked)
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != artifact.SHA256 {
		return nil, fmt.Errorf("%w: frozen artifact digest mismatch", ErrBlocked)
	}
	return data, nil
}

func credentialProvider(profile recipe.Profile) (string, error) {
	switch profile.Harness {
	case "codex":
		return "openai", nil
	case "claude-code":
		return "anthropic", nil
	case "opencode":
		provider, _, _ := strings.Cut(profile.Model, "/")
		if CredentialEnv(provider) != "" {
			return provider, nil
		}
	}
	return "", fmt.Errorf("%w: provider credential adapter unavailable", ErrBlocked)
}

// readCredential returns either the leased API key or, for Codex with
// provider openai only, the delivered ChatGPT sign-in auth.json.
func readCredential(path, attemptID, provider, harness string) ([]byte, []byte, time.Time, error) {
	if path == "" {
		return nil, nil, time.Time{}, ErrBlocked
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("%w: credential unavailable", ErrBlocked)
	}
	// The file may hold a JSON-escaped auth.json of up to 16 KiB.
	const maxFile = 64 << 10
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxFile {
		return nil, nil, time.Time{}, fmt.Errorf("%w: credential file is not private", ErrBlocked)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("%w: credential unavailable", ErrBlocked)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	defer clear(body)
	if err != nil || len(body) > maxFile {
		return nil, nil, time.Time{}, ErrBlocked
	}
	var value credential
	stale := fmt.Errorf("%w: credential is stale or mismatched", ErrBlocked)
	if err := strictJSON(body, &value); err != nil || value.AttemptID != attemptID || value.Provider != provider ||
		value.ExpiresAt <= time.Now().Unix() || value.ExpiresAt > time.Now().Add(time.Hour).Unix() {
		return nil, nil, time.Time{}, stale
	}
	expiry := time.Unix(value.ExpiresAt, 0)
	if value.CodexAuthJSON != "" {
		if value.APIKey != "" || harness != "codex" || provider != "openai" {
			return nil, nil, time.Time{}, stale
		}
		if _, err := ParseCodexAuth([]byte(value.CodexAuthJSON)); err != nil {
			return nil, nil, time.Time{}, err
		}
		return nil, []byte(value.CodexAuthJSON), expiry, nil
	}
	if len(value.APIKey) == 0 || len(value.APIKey) > 8192 || strings.ContainsAny(value.APIKey, "\r\n\x00") {
		return nil, nil, time.Time{}, stale
	}
	return []byte(value.APIKey), nil, expiry, nil
}

func strictJSON(body []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrBlocked
	}
	return nil
}

// ErrLeaseExpired stops the harness when the model lease was not renewed.
var ErrLeaseExpired = errors.New("model lease expired")

var leaseTick = 10 * time.Second

// watchLease cancels ctx once the model lease expires. The platform renews
// the lease while the attempt stays authorized and writes the new expiry to
// lease-expires in the state dir; each value is honored at most an hour ahead.
// ponytail: renewal is platform-push and the expiry file is advisory: the
// agent already holds the raw key and could rewrite the file. The real
// protection is the platform's stop-actor path on revocation, which removes
// the sandbox and the key with it.
func watchLease(ctx context.Context, expiry time.Time) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	go func() {
		for {
			if data, err := os.ReadFile(statePath("lease-expires")); err == nil {
				if n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64); err == nil {
					renewed := time.Unix(n, 0)
					if limit := time.Now().Add(time.Hour); renewed.After(limit) {
						renewed = limit
					}
					if renewed.After(expiry) {
						expiry = renewed
					}
				}
			}
			if !time.Now().Before(expiry) {
				cancel(ErrLeaseExpired)
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(min(leaseTick, time.Until(expiry)+10*time.Millisecond)):
			}
		}
	}()
	return ctx, func() { cancel(nil) }
}

// CredentialEnv is the one environment variable a provider's leased key is
// delivered in. OpenCode Zen and OpenCode Go both read OPENCODE_API_KEY
// (models.dev provider "opencode"/"opencode-go", checked 2026-09-24).
func CredentialEnv(provider string) string {
	switch provider {
	case "openai":
		return "OPENAI_API_KEY"
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "opencode", "opencode-go":
		return "OPENCODE_API_KEY"
	}
	return ""
}

// nativeCredentialEnv is where a native_raw attempt's key goes. Codex 0.156.1
// `codex exec` never sends OPENAI_API_KEY for its built-in openai provider;
// it reads CODEX_API_KEY (internal/tooladapter/codex_probe_test.go). A
// brokered attempt instead names OPENAI_API_KEY as its gateway provider's
// env_key (codexGatewayArgs).
func nativeCredentialEnv(harness, provider string) string {
	if harness == "codex" && provider == "openai" {
		return "CODEX_API_KEY"
	}
	return CredentialEnv(provider)
}
