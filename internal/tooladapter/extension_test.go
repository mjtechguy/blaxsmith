package tooladapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/extension/fixture"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// guildSource writes the Guild-shaped fixture as a plain directory, the way
// the worker's pinned checkout presents it.
func guildSource(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range fixture.GuildFiles {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func foundryMount(hooks bool, serena bool) *ExtensionMount {
	m := &ExtensionMount{ID: "guild", Version: "1.0.0", RepositoryURL: "https://github.com/alphabravo-oss/guild",
		Commit: strings.Repeat("a", 40), ManifestSHA256: strings.Repeat("c", 64), Template: "foundry-build",
		Plugins: []PluginMount{{ID: "foundry", Path: "plugins/foundry", MCPServers: []string{"foundry"}}},
		Signals: []extension.Signal{
			{Plugin: "foundry", Server: "foundry", Tool: "Foundry-Phase", Event: "phase", NameField: "phase"},
			{Plugin: "foundry", Server: "foundry", Tool: "Foundry-Handoff", Event: "handoff", NameField: "event", TextField: "summary"},
		}}
	if hooks {
		m.Plugins[0].Hooks = []PluginHook{{Event: "SessionStart", Command: "hooks/session-start-serena.sh"}}
	}
	if serena {
		m.LocalMCP = []LocalMCPServer{{Name: "serena", URL: "http://127.0.0.1:9121/mcp"}}
	}
	return m
}

func readJSON(t *testing.T, name string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestExtensionMaterializationKeepsOnlyApprovedHooksAndServers(t *testing.T) {
	source := guildSource(t)
	// Unapproved: the hook config is removed and inline hooks are stripped.
	home := t.TempDir()
	unapproved := foundryMount(false, false)
	unapproved.Plugins[0].MCPServers = nil
	roots, config, err := materializeExtension(source, home, unapproved)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "extensions", "foundry")
	if roots["foundry"] != root || config != "" {
		t.Fatalf("roots %v config %q", roots, config)
	}
	if _, err := os.Stat(filepath.Join(root, "hooks", "hooks.json")); !os.IsNotExist(err) {
		t.Fatalf("unapproved hooks.json was materialized: %v", err)
	}
	manifest := readJSON(t, filepath.Join(root, ".claude-plugin", "plugin.json"))
	if manifest["mcpServers"] != nil || manifest["hooks"] != nil || manifest["name"] != "foundry" {
		t.Fatalf("unapproved plugin.json: %v", manifest)
	}
	for _, name := range []string{"commands/start.md", "agents/teammate.md", "skills/prove/SKILL.md", "mcp-server/pyproject.toml"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("%s not materialized: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "extensions", "forge")); !os.IsNotExist(err) {
		t.Fatal("a plugin the template does not use was materialized")
	}
	// Approved: the Foundry server, the Serena hook, and the loopback Serena server.
	home = t.TempDir()
	roots, config, err = materializeExtension(source, home, foundryMount(true, true))
	if err != nil {
		t.Fatal(err)
	}
	root = roots["foundry"]
	manifest = readJSON(t, filepath.Join(root, ".claude-plugin", "plugin.json"))
	servers, _ := manifest["mcpServers"].(map[string]any)
	if servers["foundry"] == nil || len(servers) != 1 {
		t.Fatalf("approved plugin servers: %v", manifest)
	}
	hooks := readJSON(t, filepath.Join(root, "hooks", "hooks.json"))
	encoded, _ := json.Marshal(hooks)
	if !strings.Contains(string(encoded), `${CLAUDE_PLUGIN_ROOT}/hooks/session-start-serena.sh`) || !strings.Contains(string(encoded), "SessionStart") {
		t.Fatalf("approved hooks: %s", encoded)
	}
	if config != filepath.Join(home, "extension-mcp.json") {
		t.Fatalf("mcp config %q", config)
	}
	mcp := readJSON(t, config)
	if got, _ := json.Marshal(mcp); string(got) != `{"mcpServers":{"serena":{"type":"http","url":"http://127.0.0.1:9121/mcp"}}}` {
		t.Fatalf("mcp config %s", got)
	}
	if info, err := os.Stat(filepath.Join(root, "commands", "start.md")); err != nil || info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("materialized files must be read-only: %v %v", info.Mode(), err)
	}
	// A symlink in the plugin fails closed.
	if err := os.Symlink("/etc/passwd", filepath.Join(source, "plugins", "foundry", "leak")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := materializeExtension(source, t.TempDir(), foundryMount(false, false)); !errors.Is(err, ErrBlocked) {
		t.Fatalf("symlinked plugin content materialized: %v", err)
	}
}

func TestExtensionMountValidation(t *testing.T) {
	good := foundryMount(true, true)
	if err := good.validate("claude-code"); err != nil {
		t.Fatal(err)
	}
	if err := good.validate("codex"); !errors.Is(err, ErrBlocked) {
		t.Fatalf("codex ran an embedded extension: %v", err)
	}
	for name, change := range map[string]func(*ExtensionMount){
		"remote mcp":     func(m *ExtensionMount) { m.LocalMCP[0].URL = "https://mcp.example.com/mcp" },
		"escaping path":  func(m *ExtensionMount) { m.Plugins[0].Path = "../plugins" },
		"moving ref":     func(m *ExtensionMount) { m.Commit = "main" },
		"private host":   func(m *ExtensionMount) { m.RepositoryURL = "https://git.internal/guild" },
		"signal plugin":  func(m *ExtensionMount) { m.Signals[0].Plugin = "forge" },
		"no plugins":     func(m *ExtensionMount) { m.Plugins = nil },
		"hook traversal": func(m *ExtensionMount) { m.Plugins[0].Hooks[0].Command = "../../bin/sh" },
	} {
		m := foundryMount(true, true)
		change(m)
		if err := m.validate("claude-code"); !errors.Is(err, ErrBlocked) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestSignalEventsMapFoundryToolCalls(t *testing.T) {
	line := []byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"moving on"},
		{"type":"tool_use","name":"mcp__plugin_foundry_foundry__Foundry-Phase","input":{"phase":"grind_start"}},
		{"type":"tool_use","name":"mcp__plugin_foundry_foundry__Foundry-Handoff","input":{"event":"inspect_to_grind","summary":"3 defects"}},
		{"type":"tool_use","name":"mcp__plugin_foundry_foundry__Foundry-Defect","input":{"phase":"not a signal"}},
		{"type":"tool_use","name":"mcp__other__Foundry-Phase","input":{"phase":"spoofed"}}]}}`)
	var got []string
	for _, e := range signalEvents(line, foundryMount(false, false).Signals) {
		got = append(got, string(e))
	}
	want := []string{`{"name":"grind_start","source":"foundry","type":"phase"}`,
		`{"name":"inspect_to_grind","source":"foundry","text":"3 defects","type":"handoff"}`}
	if !slices.Equal(got, want) {
		t.Fatalf("signals %v", got)
	}
	if signalEvents(line, nil) != nil || signalEvents([]byte(`{"type":"user"}`), foundryMount(false, false).Signals) != nil {
		t.Fatal("signals without a declaration")
	}
}

// TestEmbeddedExtensionRunArgvAndQuestions runs a fake Claude Code under the
// real pane: the plugin comes from the attempt's temporary home, the only
// config flags are the adapter's, AskUserQuestion stays disallowed with the
// bx ask mapping in the prompt, and a Foundry-Phase call becomes a bx event.
func TestEmbeddedExtensionRunArgvAndQuestions(t *testing.T) {
	requireTmux(t)
	bin := t.TempDir()
	path := filepath.Join(bin, "claude")
	script := []byte(`#!/bin/sh
if [ "$1" = "--version" ]; then echo "2.1.280 (Claude Code)"; exit; fi
for a in "$@"; do printf '%s\n--\n' "$a"; done > argv.txt
ls "$HOME/extensions/foundry/hooks" > hooks.txt 2>&1
env | sort > env.txt
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__plugin_foundry_foundry__Foundry-Phase","input":{"phase":"inspect_start"}}]}}'
echo '{"type":"result","subtype":"success","result":"done"}'
`)
	if err := os.WriteFile(path, script, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(script)
	runtime := approved("claude-code", "claude-opus-5-5", "high")
	runtime.Binary, runtime.BinarySHA256 = path, hex.EncodeToString(sum[:])
	in, err := Prepare(runtime, recipe.Profile{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "high"},
		"Read {{plugin_root:foundry}}/commands/start.md and run it.", time.Minute, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	in = in.withExtension(foundryMount(false, false), guildSource(t))
	work := t.TempDir()
	if _, err := Run(t.Context(), in, work, []string{"ANTHROPIC_API_KEY=leased-test-key"}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(work, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Split(strings.TrimSuffix(string(data), "\n--\n"), "\n--\n")
	prompt := argv[len(argv)-1]
	pluginAt := slices.Index(argv, "--plugin-dir")
	if pluginAt < 0 || !strings.HasSuffix(argv[pluginAt+1], "/extensions/foundry") || strings.HasPrefix(argv[pluginAt+1], work) {
		t.Fatalf("plugin dir: %q", argv)
	}
	home := strings.TrimSuffix(argv[pluginAt+1], "/extensions/foundry")
	if argv[0] != "--bare" || !slices.Contains(argv, "AskUserQuestion") || argv[slices.Index(argv, "--disallowedTools")+1] != "AskUserQuestion" ||
		slices.Contains(argv, "--mcp-config") || slices.Contains(argv, "--dangerously-skip-permissions") || argv[len(argv)-2] != "--" {
		t.Fatalf("argv: %q", argv)
	}
	if !strings.HasPrefix(prompt, "Read "+home+"/extensions/foundry/commands/start.md") || strings.Contains(prompt, "{{plugin_root") ||
		!strings.Contains(prompt, "Wherever those instructions say to call AskUserQuestion, run bx ask") || !strings.Contains(prompt, "bx ask --json") {
		t.Fatalf("prompt: %q", prompt)
	}
	if hooks, _ := os.ReadFile(filepath.Join(work, "hooks.txt")); strings.Contains(string(hooks), "hooks.json") {
		t.Fatalf("unapproved hook config reached the harness: %s", hooks)
	}
	env, _ := os.ReadFile(filepath.Join(work, "env.txt"))
	for _, want := range []string{"HOME=" + home, "UV_PYTHON_INSTALL_DIR=/opt/blaxsmith/runtimes/uv/python"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Fatalf("env lacks %s: %s", want, env)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("attempt home survived the run: %v", err)
	}
	found := false
	for _, entry := range sequenced("log") {
		record, _ := os.ReadFile(entry.path)
		found = found || strings.Contains(string(record), `"event":{"name":"inspect_start","source":"foundry","type":"phase"}`)
	}
	if !found {
		t.Fatal("Foundry-Phase did not become a bx phase event")
	}
}
