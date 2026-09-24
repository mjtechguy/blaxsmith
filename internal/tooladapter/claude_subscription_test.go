package tooladapter

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func bareClaude() ([]string, []string) {
	scoped := []string{"--bare", "--settings", `{"availableModels":["claude-opus-4-6"],"fallbackModel":[]}`, "--model", "claude-opus-4-6", "--effort", "high"}
	run := append(append([]string(nil), scoped...), "--print", "--output-format", "stream-json", "--", "do it")
	return run, append([]string(nil), scoped...)
}

func TestClaudeSubscriptionModeLeavesAPIKeysAlone(t *testing.T) {
	run, resume := bareClaude()
	env := []string{"HOME=/h", "ANTHROPIC_API_KEY=sk-ant-api03-abc"}
	gotRun, gotResume, gotEnv, err := claudeSubscriptionMode("claude-code", t.TempDir(), "/h", run, resume, env)
	if err != nil || gotRun[0] != "--bare" || gotResume[0] != "--bare" || !slices.Equal(gotEnv, env) {
		t.Fatalf("API-key launch changed: %v %v %v %v", gotRun, gotResume, gotEnv, err)
	}
	gotRun, _, gotEnv, err = claudeSubscriptionMode("codex", t.TempDir(), "/h", run, resume, []string{"ANTHROPIC_API_KEY=sk-ant-oat01-x"})
	if err != nil || gotRun[0] != "--bare" || gotEnv[0] != "ANTHROPIC_API_KEY=sk-ant-oat01-x" {
		t.Fatalf("non-Claude harness changed: %v %v %v", gotRun, gotEnv, err)
	}
}

func TestClaudeSubscriptionModeRewritesArgvAndEnv(t *testing.T) {
	run, resume := bareClaude()
	home := t.TempDir()
	workdir := filepath.Join(t.TempDir(), "source")
	env := []string{"HOME=" + home, "ANTHROPIC_API_KEY=sk-ant-oat01-secret-token"}
	gotRun, gotResume, gotEnv, err := claudeSubscriptionMode("claude-code", workdir, home, run, resume, env)
	if err != nil {
		t.Fatal(err)
	}
	isolation := []string{"--setting-sources", "project", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	for _, args := range [][]string{gotRun, gotResume} {
		if !slices.Equal(args[:len(isolation)], isolation) || slices.Contains(args, "--bare") {
			t.Fatalf("isolation flags missing or --bare kept: %v", args)
		}
		i := slices.Index(args, "--settings")
		if i < 0 || !strings.HasSuffix(args[i+1], `,"disableAllHooks":true}`) {
			t.Fatalf("hooks not disabled: %v", args)
		}
	}
	if gotRun[len(gotRun)-2] != "--" || gotRun[len(gotRun)-1] != "do it" {
		t.Fatalf("prompt tail changed: %v", gotRun)
	}
	for _, want := range []string{"CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-secret-token", "CLAUDE_CONFIG_DIR=" + filepath.Join(home, ".claude"),
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"} {
		if !slices.Contains(gotEnv, want) {
			t.Fatalf("env missing %q: %v", want, gotEnv)
		}
	}
	for _, entry := range gotEnv {
		if strings.HasPrefix(entry, "ANTHROPIC_API_KEY=") {
			t.Fatalf("setup-token left in ANTHROPIC_API_KEY: %v", gotEnv)
		}
	}
}

func TestClaudeSubscriptionModeRefusesAncestorConfig(t *testing.T) {
	for _, name := range ancestorClaudeConfig {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			workdir := filepath.Join(root, "a", "source")
			if err := os.MkdirAll(workdir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			run, resume := bareClaude()
			_, _, _, err := claudeSubscriptionMode("claude-code", workdir, t.TempDir(), run, resume, []string{"ANTHROPIC_API_KEY=sk-ant-oat01-x"})
			if !errors.Is(err, ErrBlocked) {
				t.Fatalf("ancestor %s not refused: %v", name, err)
			}
		})
	}
}

func TestClaudeSetupTokenRedactedButNotAcceptedDirectly(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "sk-ant-oat01-redact-me-0123")
	if got := string(redactSecrets([]byte("tok sk-ant-oat01-redact-me-0123"), leasedSecrets())); strings.Contains(got, "redact-me") {
		t.Fatalf("setup-token not redacted: %q", got)
	}
	if slices.Contains(credentialEnvNames, "CLAUDE_CODE_OAUTH_TOKEN") {
		t.Fatal("CLAUDE_CODE_OAUTH_TOKEN must not be an accepted incoming credential variable")
	}
}
