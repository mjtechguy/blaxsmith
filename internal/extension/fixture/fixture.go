// Package fixture builds a small local Git repository shaped like the Guild
// plugins, for tests of extension install and materialization.
package fixture

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// GuildFiles mirrors the Guild layout the example manifest references.
var GuildFiles = map[string]string{
	"plugins/forge/.claude-plugin/plugin.json":      `{"name":"forge","version":"4.4.1"}`,
	"plugins/forge/commands/plan.md":                "---\ndescription: plan\n---\nUse AskUserQuestion for each round.\n",
	"plugins/forge/agents/researcher.md":            "---\nname: researcher\n---\nResearch.\n",
	"plugins/foundry/.claude-plugin/plugin.json":    `{"name":"foundry","version":"4.11.1","mcpServers":{"foundry":{"command":"uv","args":["run","--project","${CLAUDE_PLUGIN_ROOT}/mcp-server","foundry-mcp","--project-root","${CLAUDE_PROJECT_DIR}"]}}}`,
	"plugins/foundry/hooks/hooks.json":              `{"hooks":{"SessionStart":[{"matcher":".*","hooks":[{"type":"command","command":"${CLAUDE_PLUGIN_ROOT}/hooks/session-start-serena.sh","timeout":47}]}]}}`,
	"plugins/foundry/hooks/session-start-serena.sh": "#!/usr/bin/env bash\nexit 0\n",
	"plugins/foundry/commands/start.md":             "---\ndescription: start\n---\nFoundry lead.\n",
	"plugins/foundry/agents/teammate.md":            "---\nname: teammate\n---\nBuild.\n",
	"plugins/foundry/skills/prove/SKILL.md":         "---\nname: prove\ndescription: Prove.\n---\nProve it.\n",
	"plugins/foundry/mcp-server/pyproject.toml":     "[project]\nname = \"foundry-mcp\"\n",
	"README.md": "fixture\n",
}

// Repo commits files into a new repository and returns its directory and
// commit. Later calls to Commit add more commits.
func Repo(t testing.TB, files map[string]string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	return dir, Commit(t, dir, files)
}

// Commit writes files and commits them, returning the new commit.
func Commit(t testing.TB, dir string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(full, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	run(t, dir, "add", "-A")
	run(t, dir, "-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "-q", "--allow-empty", "-m", "fixture")
	return strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))
}

func run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	// No detached auto-maintenance: it races t.TempDir cleanup of .git.
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "maintenance.auto=false", "-c", "gc.auto=0"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}
