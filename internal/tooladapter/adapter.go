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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/extension"
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
	runtime         Runtime
	args            []string
	resume          []string // native resume flags; Run adds scoped paths, pane adds the session id
	base            string   // frozen commit; the pane squashes stage changes onto it
	skills          []string
	skillNames      []string
	skillArtifacts  []ArtifactDigest
	model           string        // explicit provider/model, pinned in OpenCode's config too
	timeout         time.Duration // idle timeout
	maxRuntime      time.Duration // total cap; zero is unlimited
	maxOutputBytes  int
	extension       *ExtensionMount // embedded extension stage; nil otherwise
	extensionSource string          // pinned extension checkout
	gatewayBaseURL  string          // brokered_gateway attempts only (gateway.go)
}

// withExtension binds an embedded extension stage to its pinned checkout and
// adds the AskUserQuestion to bx ask mapping to the instruction block.
func (in Invocation) withExtension(m *ExtensionMount, source string) Invocation {
	in.extension, in.extensionSource = m, source
	in.args = append([]string(nil), in.args...)
	in.args[len(in.args)-1] += extensionInstructions
	return in
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
	// Session persistence stays on (inside the attempt's temporary home) so a
	// human can resume the same native session; every other control is kept.
	prompt += bxInstructions
	var args, resume []string
	switch profile.Harness {
	case "codex":
		// `codex resume` has no --ignore-user-config; the temporary home has no
		// user config.toml, so the resumed TUI loads the same (empty) config.
		controls := []string{"--disable", "multi_agent", "--disable", "apps", "--disable", "plugins",
			"--sandbox", "workspace-write", "--model", profile.Model, "--config", `approval_policy="never"`, "--config", fmt.Sprintf("model_reasoning_effort=%q", profile.Effort),
			"--config", `web_search="disabled"`, "--config", `skills.bundled.enabled=false`}
		args = append(append([]string{"exec", "--json", "--ignore-user-config"}, controls...), prompt)
		resume = append([]string{"resume"}, controls...)
	case "claude-code":
		settings := fmt.Sprintf(`{"availableModels":[%q],"fallbackModel":[]}`, profile.Model)
		scoped := []string{"--bare", "--settings", settings, "--model", profile.Model, "--effort", profile.Effort}
		// Questions go through `bx ask` while autonomous; the native widget
		// stays available in the takeover TUI. "--" ends variadic flags.
		args = append(append([]string(nil), scoped...), "--print", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none",
			"--disallowedTools", "AskUserQuestion", "--", prompt)
		resume = scoped
	case "opencode":
		args = []string{"run", "--standalone", "--format", "json", "--model", profile.Model + "#" + profile.Effort, prompt}
		resume = []string{"--standalone", "--session"} // the session keeps its model and variant
	default:
		return Invocation{}, fmt.Errorf("%w: harness %q", ErrBlocked, profile.Harness)
	}
	return Invocation{runtime: runtime, args: args, resume: resume, skills: append([]string(nil), profile.Skills...), skillNames: skillNames, model: profile.Model, timeout: timeout, maxOutputBytes: maxOutputBytes}, nil
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
		// The model stays explicit (provider/model) in config and argv, so no
		// ambient default provider is ever picked.
		settings := map[string]any{"update": "disable", "model": in.model, "permissions": permissions}
		openCodeGatewayProvider(settings, in.gatewayBaseURL, in.model)
		data, err := json.Marshal(settings)
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
	var extensionRoots map[string]string
	var extensionMCP string
	if in.extension != nil {
		if in.runtime.Harness != "claude-code" || in.extensionSource == "" {
			return nil, fmt.Errorf("%w: embedded extension stages run only on Claude Code", ErrBlocked)
		}
		if extensionRoots, extensionMCP, err = materializeExtension(in.extensionSource, home, in.extension); err != nil {
			return nil, err
		}
	}
	env := []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=/usr/local/bin:/usr/bin:/bin", "DISABLE_UPDATES=1"}
	if in.extension != nil {
		env = append(env, extensionEnv()...)
	}
	toolEnv := append([]string(nil), env...)
	seen := map[string]bool{}
	for _, entry := range credentialEnv {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || (key != "OPENAI_API_KEY" && key != "ANTHROPIC_API_KEY" && key != "OPENCODE_API_KEY" && !gatewayEnvKey(key)) || seen[key] || strings.ContainsRune(entry, 0) {
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
	runCtx, cancelRun := context.WithCancel(ctx)
	if in.maxRuntime > 0 {
		cancelRun()
		runCtx, cancelRun = context.WithTimeout(ctx, in.maxRuntime)
	}
	defer cancelRun()
	args := in.args
	resume := append([]string(nil), in.resume...)
	switch in.runtime.Harness {
	case "claude-code":
		if in.extension != nil {
			args, resume = extensionArgs(args, resume, extensionRoots, extensionMCP, in.extension)
		}
		if claudeSkillDir != "" {
			args = append(append(append([]string(nil), args[:len(args)-2]...), "--add-dir", claudeSkillDir), args[len(args)-2:]...)
			resume = append(resume, "--add-dir", claudeSkillDir)
		}
		resume = append(resume, "--resume")
	case "codex":
		// The TUI otherwise stops at a folder-trust prompt; exec never asks.
		dir, _ := json.Marshal(workdir)
		resume = append(resume, "--config", fmt.Sprintf("projects={%s={trust_level=%q}}", dir, "trusted"))
	}
	var signals []extension.Signal
	if in.extension != nil {
		signals = in.extension.Signals
	}
	return runInPane(runCtx, in, workdir, append(toolEnv, "BLAXSMITH_STATE_DIR="+StateDir()), launch{
		Harness: in.runtime.Harness, Dir: workdir, Base: in.base, Tmux: tmuxBinary, Signals: signals,
		Run:    append([]string{in.runtime.Binary}, args...),
		Resume: append([]string{in.runtime.Binary}, resume...),
	})
}

// runInPane starts the harness in the attempt's tmux session and blocks until
// the pane (autonomous, or the human's resumed TUI) signals blaxsmith-done.
func runInPane(ctx context.Context, in Invocation, workdir string, env []string, l launch) ([]byte, error) {
	state := StateDir()
	if err := os.MkdirAll(state, 0700); err != nil {
		return nil, err
	}
	for _, name := range []string{"exit", "session-id", "events.jsonl", "stderr.log", "owner", "result.json"} {
		_ = os.Remove(filepath.Join(state, name))
	}
	data, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(state, "launch.json"), data, 0600); err != nil {
		return nil, err
	}
	socket := filepath.Join(state, "tmux.sock")
	tmux := func(ctx context.Context, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, tmuxBinary, append([]string{"-S", socket}, args...)...) // #nosec G204 -- fixed binary, separate argv
		cmd.Env, cmd.Dir = env, workdir
		cmd.WaitDelay = time.Second
		return cmd
	}
	defer func() { _ = tmux(context.Background(), "kill-server").Run() }()
	// The pane reads its argv from launch.json: tmux splits arguments that end
	// in ";", so user text must never reach tmux's command parser.
	if err := tmux(ctx, "new-session", "-d", "-s", "agent", "-x", "200", "-y", "50", "-c", workdir, "--", paneBinary, "pane").Run(); err != nil {
		return nil, fmt.Errorf("start terminal session: %w", err)
	}
	_ = tmux(ctx, "set-option", "-t", "agent", "remain-on-exit", "on").Run()
	waitCtx, stop := context.WithCancelCause(ctx)
	defer stop(nil)
	go watchIdle(waitCtx, stop, in.timeout, func() time.Time {
		out, err := tmux(waitCtx, "display-message", "-p", "-t", "agent", "#{window_activity}").Output()
		n, perr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		if err != nil || perr != nil {
			return time.Time{}
		}
		return time.Unix(n, 0)
	})
	waitErr := tmux(waitCtx, "wait-for", "blaxsmith-done").Run()
	code, err := os.ReadFile(filepath.Join(state, "exit"))
	if err != nil {
		if cause := context.Cause(waitCtx); cause != nil {
			return nil, fmt.Errorf("tool exited: %w", cause)
		}
		return nil, fmt.Errorf("terminal session ended without a result: %v", waitErr)
	}
	output := &boundedOutput{buffer: limit.Buffer{Max: in.maxOutputBytes}}
	for _, name := range []string{"events.jsonl", "stderr.log"} {
		if f, err := os.Open(filepath.Join(state, name)); err == nil {
			_, err = io.Copy(output, f)
			f.Close()
			if err != nil {
				return output.Bytes(), err
			}
		}
	}
	if status := strings.TrimSpace(string(code)); status != "0" {
		return output.Bytes(), fmt.Errorf("tool exited: exit status %s", status)
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

// ErrIdle stops a stage that made no progress for its idle timeout.
var ErrIdle = errors.New("stage idle timeout: no progress")

var idleTick = 15 * time.Second

// watchIdle stops the stage after idle with no progress: a new harness event
// line, a `bx` record, or pane output. It never stops a stage a human has
// taken over, or one waiting on a human answer (paused, as for blocked work).
func watchIdle(ctx context.Context, stop context.CancelCauseFunc, idle time.Duration, activity func() time.Time) {
	tick := time.NewTicker(max(time.Millisecond*100, min(idleTick, idle/2)))
	defer tick.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now()
		if owner, _ := os.ReadFile(statePath("owner")); string(owner) == "human" || waitingOnHuman() {
			last = now
			continue
		}
		for _, at := range []time.Time{modTime(statePath("events.jsonl")), modTime(ixPath("log")), activity()} {
			if at.After(last) {
				last = at
			}
		}
		if now.Sub(last) > idle {
			stop(ErrIdle)
			return
		}
	}
}

func modTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// waitingOnHuman reports an open `bx ask` with no answer yet.
func waitingOnHuman() bool {
	entries, _ := os.ReadDir(ixPath("asked"))
	for _, entry := range entries {
		if _, err := os.Stat(ixPath("answers", entry.Name()+".json")); err != nil {
			return true
		}
	}
	return false
}
