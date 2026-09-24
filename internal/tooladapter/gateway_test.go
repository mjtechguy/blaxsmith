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

const gatewayToken = "bxgw_testtokentesttokentesttokentesttokentes"

func gatewayFakeHarness(t *testing.T, harness, version, script string, profile recipe.Profile) Invocation {
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

// The brokered harness config: a gateway base URL and the gateway token, and
// never a provider key (docs/model-gateway-plan.md §3 step 2).
func TestGatewayHarnessConfigWriter(t *testing.T) {
	requireTmux(t)
	g := &Gateway{BaseURL: "https://gw.example"}
	t.Run("claude-code", func(t *testing.T) {
		profile := recipe.Profile{Harness: "claude-code", Model: "claude-sonnet-5", Effort: "high"}
		in := gatewayFakeHarness(t, "claude-code", "2.1.281 (Claude Code)",
			"printf '%s|%s|%s|%s' \"$ANTHROPIC_BASE_URL\" \"$ANTHROPIC_AUTH_TOKEN\" \"$ANTHROPIC_API_KEY\" \"${OPENAI_API_KEY:-}\"\n", profile)
		in.gatewayBaseURL = g.BaseURL
		output, err := Run(t.Context(), in, t.TempDir(), gatewayCredentialEnv(g, "claude-code", "anthropic", []byte(gatewayToken)))
		if err != nil {
			t.Fatalf("run: %q %v", output, err)
		}
		if got := string(output); got != "https://gw.example/anthropic|"+gatewayToken+"|"+gatewayToken+"|" {
			t.Fatalf("Claude Code env: %q", got)
		}
	})
	t.Run("codex", func(t *testing.T) {
		profile := recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "medium"}
		in := gatewayFakeHarness(t, "codex", "codex-cli 0.156.1", "printf '%s|' \"$OPENAI_API_KEY\" \"${OPENAI_BASE_URL:-}\"; printf '%s\\n' \"$@\"\n", profile)
		in.gatewayBaseURL = g.BaseURL
		output, err := Run(t.Context(), in, t.TempDir(), gatewayCredentialEnv(g, "codex", "openai", []byte(gatewayToken)))
		if err != nil {
			t.Fatalf("run: %q %v", output, err)
		}
		head, argv, _ := strings.Cut(string(output), "||")
		args := strings.Split(strings.TrimSpace(argv), "\n")
		wantProvider := `model_providers.blaxsmith-gateway={name="Blaxsmith model gateway",base_url="https://gw.example/openai/v1",env_key="OPENAI_API_KEY",wire_api="responses",supports_websockets=false}`
		if head != gatewayToken || !slices.Contains(args, `model_provider="blaxsmith-gateway"`) || !slices.Contains(args, wantProvider) ||
			strings.Index(argv, wantProvider) > strings.Index(argv, "Do the work") || args[0] != "exec" {
			t.Fatalf("Codex env/argv: %q", output)
		}
		_, resume := codexGatewayArgs(in.args, in.resume, g.BaseURL)
		if !slices.Contains(resume, wantProvider) {
			t.Fatalf("takeover resume must use the gateway too: %q", resume)
		}
	})
	t.Run("opencode", func(t *testing.T) {
		profile := recipe.Profile{Harness: "opencode", Model: "opencode/kimi-k3", Effort: "high"}
		in := gatewayFakeHarness(t, "opencode", "opencode v2.0.14",
			"printf '%s@@' \"$OPENCODE_API_KEY\"; cat \"$XDG_CONFIG_HOME/opencode/opencode.json\"\n", profile)
		in.gatewayBaseURL = g.BaseURL
		output, err := Run(t.Context(), in, t.TempDir(), gatewayCredentialEnv(g, "opencode", "opencode", []byte(gatewayToken)))
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
		if err := json.Unmarshal([]byte(raw), &config); err != nil || key != gatewayToken || config.Model != "opencode/kimi-k3" ||
			config.Provider["opencode"].Options["baseURL"] != "https://gw.example/opencode/v1" ||
			config.Provider["opencode"].Options["apiKey"] != "{env:OPENCODE_API_KEY}" || strings.Contains(raw, gatewayToken) {
			t.Fatalf("OpenCode config: key=%q %s %v", key, raw, err)
		}
	})
	t.Run("native attempts are unchanged", func(t *testing.T) {
		if env := gatewayCredentialEnv(g, "codex", "openai", []byte("k")); !slices.Equal(env, []string{"OPENAI_API_KEY=k"}) {
			t.Fatalf("codex env: %q", env)
		}
		args, resume := codexGatewayArgs([]string{"exec", "p"}, []string{"resume"}, "")
		if !slices.Equal(args, []string{"exec", "p"}) || !slices.Equal(resume, []string{"resume"}) {
			t.Fatal("native Codex argv changed")
		}
		config := map[string]any{}
		openCodeGatewayProvider(config, "", "opencode/x")
		if len(config) != 0 {
			t.Fatal("native OpenCode config changed")
		}
	})
	t.Run("gateway URL validation", func(t *testing.T) {
		for _, bad := range []string{"ftp://gw", "https://gw/path", "https://user@gw", "https://gw?x=1", `https://gw"x`, "https://"} {
			if err := (&Gateway{BaseURL: bad}).validate(); !errors.Is(err, ErrBlocked) {
				t.Errorf("%q accepted", bad)
			}
		}
		if err := (&Gateway{BaseURL: "http://blaxsmith-gw.blaxsmith.svc:8443"}).validate(); err != nil {
			t.Fatal(err)
		}
	})
}
