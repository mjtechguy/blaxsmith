// Package tooladapter prepares one pinned, noninteractive CLI attempt. It does
// not authorize a run, inject credentials, or interpret agent output as success.
package tooladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/limit"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

var (
	imageDigest = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	sha256Hex   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	version     = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
	selection   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	effort      = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	ErrBlocked  = errors.New("tool capability unsupported")
)

// Runtime is an administrator-approved image/executable record, never a value
// copied from a Git recipe or the live npm catalog.
type Runtime struct {
	Harness      string
	Image        string
	Binary       string
	BinarySHA256 string
	Version      string
	Supported    []ModelEffort
}

type ModelEffort struct{ Model, Effort string }

type Invocation struct {
	runtime        Runtime
	args           []string
	skills         []string
	skillNames     []string
	skillArtifacts []ArtifactDigest
	timeout        time.Duration
	maxOutputBytes int
}

func (in Invocation) Image() string { return in.runtime.Image }

// Prepare rejects every unapproved or unrepresentable choice. The returned
// image must also be enforced by AX's runner-image gate at dispatch.
func Prepare(runtime Runtime, profile recipe.Profile, prompt string, timeout time.Duration, maxOutputBytes int) (Invocation, error) {
	if runtime.Harness != profile.Harness || !imageDigest.MatchString(runtime.Image) ||
		!filepath.IsAbs(runtime.Binary) || !sha256Hex.MatchString(runtime.BinarySHA256) ||
		!version.MatchString(runtime.Version) || timeout < time.Second || timeout > 24*time.Hour ||
		maxOutputBytes < 1 || maxOutputBytes > 16<<20 || prompt == "" || len(prompt) > 1<<20 ||
		!selection.MatchString(profile.Model) || !effort.MatchString(profile.Effort) || len(profile.Instructions) != 0 {
		return Invocation{}, fmt.Errorf("%w: invalid runtime, limits, prompt, or unsupported instructions", ErrBlocked)
	}
	skillRoots, err := skillFilesByRoot(profile.Skills)
	if err != nil {
		return Invocation{}, err
	}
	skillNames := make([]string, 0, len(skillRoots))
	for root := range skillRoots {
		skillNames = append(skillNames, filepath.Base(filepath.FromSlash(root)))
	}
	sort.Strings(skillNames)
	if profile.Harness == "opencode" {
		provider, model, ok := strings.Cut(profile.Model, "/")
		if !ok || provider == "" || model == "" || strings.Contains(model, "/") || profile.Effort == "provider-default" {
			return Invocation{}, fmt.Errorf("%w: OpenCode needs an explicit provider/model and variant", ErrBlocked)
		}
	}
	if profile.Harness == "claude-code" && !strings.HasPrefix(profile.Model, "claude-") {
		return Invocation{}, fmt.Errorf("%w: Claude aliases and unversioned model names are not pinned", ErrBlocked)
	}
	approved := false
	for _, pair := range runtime.Supported {
		approved = approved || pair == (ModelEffort{profile.Model, profile.Effort})
	}
	if !approved {
		return Invocation{}, fmt.Errorf("%w: %s %s/%s on %s", ErrBlocked, profile.Harness, profile.Model, profile.Effort, runtime.Version)
	}
	var args []string
	switch profile.Harness {
	case "codex":
		args = []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--disable", "multi_agent", "--disable", "apps", "--disable", "plugins",
			"--sandbox", "workspace-write", "--model", profile.Model, "--config", `approval_policy="never"`, "--config", fmt.Sprintf("model_reasoning_effort=%q", profile.Effort),
			"--config", `web_search="disabled"`, "--config", `skills.bundled.enabled=false`, prompt}
	case "claude-code":
		settings := fmt.Sprintf(`{"availableModels":[%q],"fallbackModel":[]}`, profile.Model)
		args = []string{"--bare", "--print", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--no-session-persistence",
			"--settings", string(settings), "--model", profile.Model, "--effort", profile.Effort, prompt}
	case "opencode":
		args = []string{"run", "--standalone", "--format", "json", "--model", profile.Model + "#" + profile.Effort, prompt}
	default:
		return Invocation{}, fmt.Errorf("%w: harness %q", ErrBlocked, profile.Harness)
	}
	return Invocation{runtime: runtime, args: args, skills: append([]string(nil), profile.Skills...), skillNames: skillNames, timeout: timeout, maxOutputBytes: maxOutputBytes}, nil
}

// Run checks the executable bytes and version in the pod, then bounds elapsed
// time and combined stdout/stderr. credentialEnv must come from a scoped lease;
// this package deliberately has no ambient environment inheritance.
func Run(ctx context.Context, in Invocation, workdir string, credentialEnv []string) ([]byte, error) {
	if !filepath.IsAbs(workdir) || !filepath.IsAbs(in.runtime.Binary) || !sha256Hex.MatchString(in.runtime.BinarySHA256) ||
		in.timeout < time.Second || in.timeout > 24*time.Hour || in.maxOutputBytes < 1 || in.maxOutputBytes > 16<<20 || len(in.args) == 0 {
		return nil, fmt.Errorf("%w: invalid invocation", ErrBlocked)
	}
	if err := rejectProjectConfig(workdir, in.runtime.Harness); err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "blaxsmith-tool-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	if in.runtime.Harness == "opencode" {
		config := filepath.Join(home, ".config", "opencode")
		if err := os.MkdirAll(config, 0700); err != nil {
			return nil, err
		}
		skillFiles := filepath.Join(config, "skills")
		permissions := []map[string]string{
			{"action": "*", "resource": "*", "effect": "allow"},
			{"action": "subagent", "resource": "*", "effect": "deny"},
			{"action": "question", "resource": "*", "effect": "deny"},
			{"action": "webfetch", "resource": "*", "effect": "deny"},
			{"action": "websearch", "resource": "*", "effect": "deny"},
			{"action": "execute", "resource": "*", "effect": "deny"},
			{"action": "skill", "resource": "*", "effect": "deny"},
			{"action": "external_directory", "resource": "*", "effect": "deny"},
		}
		if len(in.skillNames) > 0 {
			for _, name := range in.skillNames {
				root := filepath.ToSlash(filepath.Join(skillFiles, name, "*"))
				permissions = append(permissions,
					map[string]string{"action": "external_directory", "resource": root, "effect": "allow"},
					map[string]string{"action": "read", "resource": root, "effect": "allow"},
					map[string]string{"action": "edit", "resource": root, "effect": "deny"},
					map[string]string{"action": "skill", "resource": name, "effect": "allow"},
				)
			}
		}
		// OpenCode uses the last matching rule, so keep env protection after
		// selected-skill read grants.
		permissions = append(permissions,
			map[string]string{"action": "read", "resource": "*.env", "effect": "deny"},
			map[string]string{"action": "read", "resource": "*.env.*", "effect": "deny"},
			map[string]string{"action": "read", "resource": "*.env.example", "effect": "allow"},
		)
		data, err := json.Marshal(map[string]any{"update": "disable", "permissions": permissions})
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(config, "opencode.json"), data, 0600); err != nil {
			return nil, err
		}
	}
	var claudeSkillDir string
	if len(in.skills) > 0 {
		if len(in.skillArtifacts) == 0 {
			return nil, fmt.Errorf("%w: selected skills are not bound to frozen artifacts", ErrBlocked)
		}
		claudeSkillDir, err = materializeSkills(workdir, home, in.runtime.Harness, in.skills, in.skillArtifacts)
		if err != nil {
			return nil, err
		}
	}
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=/usr/local/bin:/usr/bin:/bin", "DISABLE_UPDATES=1"}
	toolEnv := append([]string(nil), env...)
	seen := map[string]bool{}
	for _, entry := range credentialEnv {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || (key != "OPENAI_API_KEY" && key != "ANTHROPIC_API_KEY") || seen[key] || strings.ContainsRune(entry, 0) {
			return nil, fmt.Errorf("%w: forbidden environment key", ErrBlocked)
		}
		seen[key] = true
		toolEnv = append(toolEnv, entry)
	}
	binary, err := os.Open(in.runtime.Binary)
	if err != nil {
		return nil, err
	}
	defer binary.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, binary); err != nil {
		return nil, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != in.runtime.BinarySHA256 {
		return nil, fmt.Errorf("%w: executable digest mismatch", ErrBlocked)
	}
	versionCtx, cancelVersion := context.WithTimeout(ctx, 20*time.Second)
	defer cancelVersion()
	check := exec.CommandContext(versionCtx, in.runtime.Binary, "--version") // #nosec G204 -- verified absolute executable, separate argv
	check.Env, check.Dir = env, workdir
	check.WaitDelay = time.Second
	versionOutput := &boundedOutput{buffer: limit.Buffer{Max: 4096}}
	check.Stdout, check.Stderr = versionOutput, versionOutput
	if err := check.Run(); err != nil || strings.TrimSpace(versionOutput.String()) != expectedVersion(in.runtime) {
		return nil, fmt.Errorf("%w: executable version mismatch: %v", ErrBlocked, err)
	}
	runCtx, cancelRun := context.WithTimeout(ctx, in.timeout)
	defer cancelRun()
	args := in.args
	if in.runtime.Harness == "claude-code" && claudeSkillDir != "" {
		args = append(append(append([]string(nil), args[:len(args)-1]...), "--add-dir", claudeSkillDir), args[len(args)-1])
	}
	cmd := exec.CommandContext(runCtx, in.runtime.Binary, args...) // #nosec G204 -- verified absolute executable, separate argv
	cmd.Env, cmd.Dir = toolEnv, workdir
	cmd.WaitDelay = time.Second
	output := &boundedOutput{buffer: limit.Buffer{Max: in.maxOutputBytes}}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Run(); err != nil {
		return output.Bytes(), fmt.Errorf("tool exited: %w", err)
	}
	return output.Bytes(), nil
}

type boundedOutput struct {
	mu     sync.Mutex
	buffer limit.Buffer
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *boundedOutput) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buffer.Bytes()...)
}

func (b *boundedOutput) String() string { return string(b.Bytes()) }

func expectedVersion(runtime Runtime) string {
	switch runtime.Harness {
	case "codex":
		return "codex-cli " + runtime.Version
	case "claude-code":
		return runtime.Version + " (Claude Code)"
	case "opencode":
		return "opencode v" + runtime.Version
	default:
		return ""
	}
}

// CLIs must not silently acquire a project-local plugin, MCP server, or
// fallback policy. Bundled instructions and skills need an explicit adapter.
func rejectProjectConfig(workdir, harness string) error {
	resolved, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return err
	}
	for dir := resolved; ; dir = filepath.Dir(dir) {
		var names []string
		switch harness {
		case "codex":
			names = []string{".codex"}
		case "opencode":
			names = []string{".opencode", "opencode.json", "opencode.jsonc"}
		}
		for _, name := range names {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				return fmt.Errorf("%w: project CLI configuration is not approved", ErrBlocked)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	return nil
}
