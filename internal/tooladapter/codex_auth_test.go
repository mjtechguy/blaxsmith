package tooladapter

import (
	"bytes"
	"context"
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

func codexAuthFile(t *testing.T, access, refresh string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"OPENAI_API_KEY": nil, "last_refresh": "2026-09-24T00:00:00Z",
		"tokens": map[string]string{"id_token": "id-" + access, "access_token": access, "refresh_token": refresh, "account_id": "acct"}})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A delivered Codex sign-in lands at $CODEX_HOME/auth.json (0600) inside the
// attempt's temp home. It never reaches the environment or argv, and its
// tokens are redacted from the returned output.
func TestCodexSignInLandsInCodexHomeNeverEnvOrArgv(t *testing.T) {
	requireTmux(t)
	root := t.TempDir()
	binary := filepath.Join(root, "codex")
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.156.1'; exit; fi\n" +
		"env > \"$PWD/env.txt\"\nprintf '%s\\n' \"$@\" > \"$PWD/argv.txt\"\nls -l \"$CODEX_HOME/auth.json\" > \"$PWD/mode.txt\"\n" +
		"cat \"$BLAXSMITH_STATE_DIR/codex-auth-path\" > \"$PWD/path.txt\"\n" +
		"printf 'key=%s home=%s ' \"${OPENAI_API_KEY:-}\" \"$CODEX_HOME\"\ncat \"$CODEX_HOME/auth.json\"\n")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	request := Request{AttemptID: "attempt-1", RepositoryURL: "https://github.com/owner/repo", SourceCommit: strings.Repeat("a", 40),
		Runtime: Runtime{Harness: "codex", Image: "example/tool@sha256:" + strings.Repeat("a", 64),
			Binary: binary, BinarySHA256: hex.EncodeToString(hash[:]), Version: "0.156.1",
			Supported: []ModelEffort{{Model: "gpt-6-luna", Effort: "xhigh"}}},
		Profile: recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"},
		Prompt:  "Build this", TimeoutSeconds: 60, MaxOutputBytes: 1 << 16}
	command, err := Command(request)
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(root, "credential.json")
	write := func(value credential) {
		t.Helper()
		data, _ := json.Marshal(value)
		if err := os.WriteFile(credentialPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	checkout := func(context.Context, string, string, string, string) error { return nil }
	expiry := time.Now().Add(10 * time.Minute).Unix()
	const access = "chatgpt-access-token-0123456789"
	for name, value := range map[string]credential{
		"refresh token":    {AttemptID: "attempt-1", Provider: "openai", ExpiresAt: expiry, CodexAuthJSON: codexAuthFile(t, access, "rt-live")},
		"with api key":     {AttemptID: "attempt-1", Provider: "openai", ExpiresAt: expiry, APIKey: "sk-x", CodexAuthJSON: codexAuthFile(t, access, "")},
		"no access token":  {AttemptID: "attempt-1", Provider: "openai", ExpiresAt: expiry, CodexAuthJSON: codexAuthFile(t, "", "")},
		"other attempt":    {AttemptID: "attempt-2", Provider: "openai", ExpiresAt: expiry, CodexAuthJSON: codexAuthFile(t, access, "")},
		"other provider":   {AttemptID: "attempt-1", Provider: "anthropic", ExpiresAt: expiry, CodexAuthJSON: codexAuthFile(t, access, "")},
		"expired sign-in":  {AttemptID: "attempt-1", Provider: "openai", ExpiresAt: time.Now().Add(-time.Minute).Unix(), CodexAuthJSON: codexAuthFile(t, access, "")},
		"not a sign-in":    {AttemptID: "attempt-1", Provider: "openai", ExpiresAt: expiry, CodexAuthJSON: `{"OPENAI_API_KEY":"sk-x"}`},
		"trailing content": {AttemptID: "attempt-1", Provider: "openai", ExpiresAt: expiry, CodexAuthJSON: codexAuthFile(t, access, "") + "{}"},
	} {
		write(value)
		if _, err := execute(t.Context(), command[1], root, credentialPath, checkout); !errors.Is(err, ErrBlocked) {
			t.Fatalf("%s: Codex sign-in must block: %v", name, err)
		}
	}
	write(credential{AttemptID: "attempt-1", Provider: "openai", ExpiresAt: expiry, CodexAuthJSON: codexAuthFile(t, access, "")})
	output, err := execute(t.Context(), command[1], root, credentialPath, checkout)
	if err != nil {
		t.Fatalf("codex sign-in run: %q %v", output, err)
	}
	if strings.Contains(string(output), access) || !strings.Contains(string(output), "[redacted]") || !strings.Contains(string(output), "key= home=") {
		t.Fatalf("output leaked the sign-in or used an API key: %q", output)
	}
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	env, argv, mode, path := read("env.txt"), read("argv.txt"), read("mode.txt"), strings.TrimSpace(read("path.txt"))
	if strings.Contains(env, access) || strings.Contains(argv, access) || strings.Contains(env, "OPENAI_API_KEY") {
		t.Fatalf("sign-in reached env or argv:\nenv=%s\nargv=%s", env, argv)
	}
	var codexHome string
	for _, line := range strings.Split(env, "\n") {
		if value, ok := strings.CutPrefix(line, "CODEX_HOME="); ok {
			codexHome = value
		}
	}
	home := filepath.Dir(codexHome)
	if filepath.Base(codexHome) != ".codex" || !strings.HasPrefix(filepath.Base(home), "blaxsmith-tool-") ||
		!strings.Contains(env, "HOME="+home+"\n") || path != filepath.Join(codexHome, "auth.json") {
		t.Fatalf("CODEX_HOME %q is not inside the attempt's temp home (recorded %q):\n%s", codexHome, path, env)
	}
	if !strings.HasPrefix(mode, "-rw-------") || strings.HasPrefix(mode, "-rw-------x") { // macOS may append @ for xattrs
		t.Fatalf("auth.json mode: %s", mode)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("temp home with the sign-in survived the run: %v", err)
	}
	if _, err := os.Stat(statePath(CodexAuthPathFile)); !os.IsNotExist(err) {
		t.Fatalf("renewal path record survived the run: %v", err)
	}
}

// A sign-in is only for the Codex harness and never alongside an env key.
func TestCodexSignInOnlyForCodexAlone(t *testing.T) {
	runtime := Runtime{Harness: "opencode", Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: "/bin/true",
		BinarySHA256: strings.Repeat("b", 64), Version: "2.0.14", Supported: []ModelEffort{{Model: "openai/gpt-6-luna", Effort: "high"}}}
	in, err := Prepare(runtime, recipe.Profile{Harness: "opencode", Model: "openai/gpt-6-luna", Effort: "high"}, "work", time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	auth := []byte(codexAuthFile(t, "chatgpt-access-token-0123456789", ""))
	// Each case must stop at the sign-in check, before the (bogus) binary digest.
	blocked := func(err error, reason string) bool {
		return errors.Is(err, ErrBlocked) && strings.Contains(err.Error(), reason)
	}
	if _, err := Run(t.Context(), in.withCodexAuth(auth), t.TempDir(), nil); !blocked(err, "only for Codex") {
		t.Fatalf("OpenCode received a Codex sign-in: %v", err)
	}
	runtime.Harness, runtime.Version, runtime.Supported = "codex", "0.156.1", []ModelEffort{{Model: "gpt-6-luna", Effort: "high"}}
	in, err = Prepare(runtime, recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "high"}, "work", time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), in.withCodexAuth(auth), t.TempDir(), []string{"OPENAI_API_KEY=sk-x"}); !blocked(err, "only for Codex") {
		t.Fatalf("Codex sign-in alongside an API key: %v", err)
	}
	if _, err := Run(t.Context(), in.withCodexAuth([]byte(codexAuthFile(t, "tok-0123456789", "rt"))), t.TempDir(), nil); !blocked(err, "invalid Codex sign-in") {
		t.Fatalf("refresh-token-bearing sign-in accepted: %v", err)
	}
}

// Pane and activity redaction hide the sign-in's tokens, including ones the
// platform writes in place at renewal, and the resume pane skips the API-key
// login so it never overwrites the sign-in.
func TestCodexSignInRedactedAndResumeSkipsAPIKeyLogin(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	t.Setenv("OPENAI_API_KEY", "")
	path := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(path, []byte(codexAuthFile(t, "first-access-token-0123", "")), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := (redactor{&out}).Write([]byte("tok first-access-token-0123 id-first-access-token-0123\n")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "first-access") {
		t.Fatalf("pane leaked the access token: %q", out.String())
	}
	// Renewal replaces the file in place with a later mtime.
	if err := os.WriteFile(path, []byte(codexAuthFile(t, "second-access-token-0123", "")), 0600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if got := redactText("a second-access-token-0123 b first-access-token-0123"); strings.Contains(got, "access-token") {
		t.Fatalf("activity leaked a renewed or replaced token: %q", got)
	}
	marker := filepath.Join(t.TempDir(), "login-ran")
	binary := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	prepareInteractive(launch{Harness: "codex", Run: []string{binary}})
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("resume pane ran an API-key login over the Codex sign-in")
	}
	t.Setenv("OPENAI_API_KEY", "sk-leased-key")
	prepareInteractive(launch{Harness: "codex", Run: []string{binary}})
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("an API-key lease must still log the resume pane in")
	}
}
