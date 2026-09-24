package tooladapter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// An OpenCode Zen key reaches the harness only as OPENCODE_API_KEY, with the
// provider/model explicit in argv and config, and no other provider key.
func TestOpenCodeZenKeyReachesOnlyOpenCodeEnv(t *testing.T) {
	requireTmux(t)
	profile := recipe.Profile{Harness: "opencode", Model: "opencode/gpt-5.1-codex", Effort: "high"}
	provider, err := credentialProvider(profile)
	if err != nil || provider != "opencode" || CredentialEnv(provider) != "OPENCODE_API_KEY" ||
		CredentialEnv("opencode-go") != "OPENCODE_API_KEY" || CredentialEnv("gemini") != "" {
		t.Fatalf("OpenCode provider credential mapping: %q %v", provider, err)
	}
	if provider, err := credentialProvider(recipe.Profile{Harness: "opencode", Model: "opencode-go/kimi-k2", Effort: "high"}); err != nil || provider != "opencode-go" {
		t.Fatalf("OpenCode Go provider credential mapping: %q %v", provider, err)
	}
	if _, err := credentialProvider(recipe.Profile{Harness: "opencode", Model: "gemini/pro", Effort: "high"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("unknown OpenCode provider must block: %v", err)
	}
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'opencode v2.0.14'; exit; fi\n" +
		"printf '%s|%s|%s@@CONFIG@@' \"$OPENCODE_API_KEY\" \"${OPENAI_API_KEY:-}\" \"$5 $6\"\ncat \"$XDG_CONFIG_HOME/opencode/opencode.json\"\n")
	binary := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	runtime := Runtime{Harness: "opencode", Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
		BinarySHA256: hex.EncodeToString(sum[:]), Version: "2.0.14", Supported: []ModelEffort{{Model: profile.Model, Effort: "high"}}}
	in, err := Prepare(runtime, profile, "Do the work", time.Minute, 16384)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"run", "--standalone", "--format", "json", "--model", "opencode/gpt-5.1-codex#high"}
	for i, arg := range wantArgs {
		if in.args[i] != arg {
			t.Fatalf("OpenCode argv: %q", in.args)
		}
	}
	output, err := Run(t.Context(), in, t.TempDir(), []string{"OPENCODE_API_KEY=zen-leased-key"})
	if err != nil {
		t.Fatalf("run: %q %v", output, err)
	}
	head, configJSON, _ := strings.Cut(string(output), "@@CONFIG@@")
	fields := strings.Split(head, "|")
	if len(fields) != 3 || fields[0] != "zen-leased-key" || fields[1] != "" || fields[2] != "--model opencode/gpt-5.1-codex#high" {
		t.Fatalf("OpenCode env/argv: %q", output)
	}
	var config struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(configJSON), &config); err != nil || config.Model != "opencode/gpt-5.1-codex" {
		t.Fatalf("OpenCode config must pin the explicit provider/model: %s %v", configJSON, err)
	}
	if _, err := Run(t.Context(), in, t.TempDir(), []string{"OPENCODE_API_KEY=a", "OPENCODE_API_KEY=b"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("duplicate credential env must block: %v", err)
	}
}
