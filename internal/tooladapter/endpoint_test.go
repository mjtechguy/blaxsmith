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

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func endpointFakeHarness(t *testing.T, harness, version, script string, profile recipe.Profile) Invocation {
	t.Helper()
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '" + version + "'; exit; fi\n" + script)
	binary := filepath.Join(t.TempDir(), harness)
	if err := os.WriteFile(binary, body, 0o700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	v := strings.Fields(strings.TrimPrefix(strings.TrimPrefix(version, "codex-cli "), "opencode v"))[0]
	runtime := Runtime{Harness: harness, Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
		BinarySHA256: hex.EncodeToString(sum[:]), Version: v, Supported: []ModelEffort{{Model: profile.Model, Effort: profile.Effort}}}
	in, err := Prepare(runtime, profile, "Do the work", time.Minute, 1<<16)
	if err != nil {
		t.Fatal(err)
	}
	return in
}

// The exact environment and config each harness gets for a connection base
// URL, through the same Run path as a real attempt.
func TestModelBaseURLHarnessConfig(t *testing.T) {
	requireTmux(t)
	t.Run("claude-code", func(t *testing.T) {
		const base = "https://litellm.example.com"
		profile := recipe.Profile{Harness: "claude-code", Model: "claude-sonnet-5", Effort: "high"}
		in := endpointFakeHarness(t, "claude-code", "2.1.281 (Claude Code)",
			"printf '%s|%s|%s|%s' \"$ANTHROPIC_BASE_URL\" \"$ANTHROPIC_API_KEY\" \"${ANTHROPIC_AUTH_TOKEN:-}\" \"${OPENAI_BASE_URL:-}\"\n", profile)
		in.modelBaseURL = base
		env, err := modelEnv("claude-code", "anthropic", base, []byte("sk-ant-key"))
		if err != nil || !slices.Equal(env, []string{"ANTHROPIC_API_KEY=sk-ant-key", "ANTHROPIC_BASE_URL=" + base}) {
			t.Fatalf("Claude Code env: %q %v", env, err)
		}
		output, err := Run(t.Context(), in, t.TempDir(), env)
		if err != nil {
			t.Fatalf("run: %q %v", output, err)
		}
		if got := string(output); got != base+"|sk-ant-key||" {
			t.Fatalf("Claude Code saw: %q", got)
		}
	})
	t.Run("codex", func(t *testing.T) {
		const base = "https://litellm.example.com/v1"
		profile := recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "medium"}
		in := endpointFakeHarness(t, "codex", "codex-cli 0.156.1",
			"printf '%s|%s|' \"${CODEX_API_KEY:-}\" \"${OPENAI_BASE_URL:-}\"; test -n \"$OPENAI_API_KEY\" && printf 'key|'; printf '%s\\n' \"$@\"\n", profile)
		in.modelBaseURL = base
		env, err := modelEnv("codex", "openai", base, []byte("sk-openai"))
		if err != nil || !slices.Equal(env, []string{"OPENAI_API_KEY=sk-openai"}) {
			t.Fatalf("Codex env: %q %v", env, err)
		}
		output, err := Run(t.Context(), in, t.TempDir(), env)
		if err != nil {
			t.Fatalf("run: %q %v", output, err)
		}
		head, argv, _ := strings.Cut(string(output), "||key|")
		args := strings.Split(strings.TrimSpace(argv), "\n")
		wantProvider := `model_providers.blaxsmith-endpoint={name="Custom endpoint",base_url="https://litellm.example.com/v1",env_key="OPENAI_API_KEY",wire_api="responses",supports_websockets=false}`
		if head != "" || !slices.Contains(args, `model_provider="blaxsmith-endpoint"`) || !slices.Contains(args, wantProvider) ||
			strings.Index(argv, wantProvider) > strings.Index(argv, "Do the work") || args[0] != "exec" {
			t.Fatalf("Codex env/argv: %q", output)
		}
		_, resume := codexEndpointArgs(in.args, in.resume, base)
		if !slices.Contains(resume, wantProvider) {
			t.Fatalf("takeover resume must use the endpoint too: %q", resume)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		const base = "https://litellm.example.com/v1"
		profile := recipe.Profile{Harness: "opencode", Model: "anthropic/claude-sonnet-5", Effort: "high"}
		in := endpointFakeHarness(t, "opencode", "opencode v2.0.14",
			"test -n \"$ANTHROPIC_API_KEY\" && printf 'key'; printf '@@'; cat \"$XDG_CONFIG_HOME/opencode/opencode.json\"\n", profile)
		in.modelBaseURL = base
		env, err := modelEnv("opencode", "anthropic", base, []byte("sk-ant-key"))
		if err != nil || !slices.Equal(env, []string{"ANTHROPIC_API_KEY=sk-ant-key"}) {
			t.Fatalf("OpenCode env: %q %v", env, err)
		}
		output, err := Run(t.Context(), in, t.TempDir(), env)
		if err != nil {
			t.Fatalf("run: %q %v", output, err)
		}
		key, raw, _ := strings.Cut(string(output), "@@")
		var config struct {
			Model    string `json:"model"`
			Provider map[string]struct {
				Options map[string]string `json:"options"`
			} `json:"provider"`
		}
		if err := json.Unmarshal([]byte(raw), &config); err != nil || key != "key" || config.Model != "anthropic/claude-sonnet-5" ||
			config.Provider["anthropic"].Options["baseURL"] != base ||
			config.Provider["anthropic"].Options["apiKey"] != "{env:ANTHROPIC_API_KEY}" || strings.Contains(raw, "sk-ant-key") {
			t.Fatalf("OpenCode config: key=%q %s %v", key, raw, err)
		}
	})
	t.Run("no base URL leaves every harness native", func(t *testing.T) {
		for harness, want := range map[string]string{"codex": "CODEX_API_KEY=k", "claude-code": "ANTHROPIC_API_KEY=k"} {
			provider := map[string]string{"codex": "openai", "claude-code": "anthropic"}[harness]
			if env, err := modelEnv(harness, provider, "", []byte("k")); err != nil || !slices.Equal(env, []string{want}) {
				t.Fatalf("%s env: %q %v", harness, env, err)
			}
		}
		args, resume := codexEndpointArgs([]string{"exec", "p"}, []string{"resume"}, "")
		if !slices.Equal(args, []string{"exec", "p"}) || !slices.Equal(resume, []string{"resume"}) {
			t.Fatal("native Codex argv changed")
		}
		config := map[string]any{}
		openCodeEndpointProvider(config, "", "opencode/x")
		if len(config) != 0 {
			t.Fatal("native OpenCode config changed")
		}
	})
	t.Run("a subscription never takes a base URL", func(t *testing.T) {
		if _, err := modelEnv("claude-code", "anthropic", "https://litellm.example.com", []byte(claudeSetupTokenPrefix+"01-x")); !errors.Is(err, ErrBlocked) {
			t.Fatalf("setup-token with a base URL: %v", err)
		}
	})
}

func TestModelBaseURLIsPublicConfiguration(t *testing.T) {
	if !endpointEnvKey("ANTHROPIC_BASE_URL") || endpointEnvKey("ANTHROPIC_AUTH_TOKEN") || endpointEnvKey("OPENAI_API_KEY") {
		t.Fatal("endpoint env allowlist")
	}
	for _, name := range []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "ANTHROPIC_AUTH_TOKEN"} {
		if slices.Contains(credentialEnvNames, name) {
			t.Fatalf("%s must not be a redacted credential variable", name)
		}
	}
	for _, bad := range []string{"http://litellm.example.com", "ftp://x", "https://user@x.example", "https://x.example?a=1",
		"https://x.example#f", "https://x.example/", `https://x.example/"}`, "https://", "https://x.example/a b",
		"https://" + strings.Repeat("a", 520) + ".example"} {
		if err := validModelBaseURL(bad); !errors.Is(err, ErrBlocked) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"", "https://litellm.example.com", "https://gw.example.com:8443/v1"} {
		if err := validModelBaseURL(good); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
}
