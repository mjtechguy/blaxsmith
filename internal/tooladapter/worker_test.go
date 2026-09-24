package tooladapter

import (
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

func TestAXWorkerRequiresScopedCredentialBeforePinnedTool(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codex")
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.156.1'; exit; fi\nprintf '%s|%s|%s' \"$OPENAI_API_KEY\" \"$7\" \"${SHOULD_NOT_LEAK:-}\"\n")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	request := Request{AttemptID: "attempt-1", RepositoryURL: "https://github.com/owner/repo", SourceCommit: strings.Repeat("a", 40),
		Runtime: Runtime{Harness: "codex", Image: "example/tool@sha256:" + strings.Repeat("a", 64),
			Binary: binary, BinarySHA256: hex.EncodeToString(hash[:]), Version: "0.156.1",
			Supported: []ModelEffort{{Model: "gpt-6-luna", Effort: "xhigh"}}},
		Profile: recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"},
		Prompt:  "Build this", TimeoutSeconds: 60, MaxOutputBytes: 1024}
	command, err := Command(request)
	if err != nil || len(command) != 2 || command[0] != WorkerBinary || strings.Contains(command[1], "leased-secret") {
		t.Fatalf("invalid public AX command: %q: %v", command, err)
	}
	credentialPath := filepath.Join(root, "credential.json")
	checkout := func(context.Context, string, string, string, string) error { return nil }
	if _, err := execute(context.Background(), command[1], root, credentialPath, checkout); !errors.Is(err, ErrBlocked) {
		t.Fatalf("missing credential must block: %v", err)
	}
	writeCredential := func(attempt, provider string, expiry int64) {
		t.Helper()
		data, _ := json.Marshal(credential{AttemptID: attempt, Provider: provider, ExpiresAt: expiry, APIKey: "leased-secret"})
		if err := os.WriteFile(credentialPath, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeCredential("wrong-attempt", "openai", time.Now().Add(10*time.Minute).Unix())
	if _, err := execute(context.Background(), command[1], root, credentialPath, checkout); !errors.Is(err, ErrBlocked) {
		t.Fatalf("wrong attempt must block: %v", err)
	}
	writeCredential("attempt-1", "openai", time.Now().Add(-time.Minute).Unix())
	if _, err := execute(context.Background(), command[1], root, credentialPath, checkout); !errors.Is(err, ErrBlocked) {
		t.Fatalf("expired credential must block: %v", err)
	}
	writeCredential("attempt-1", "openai", time.Now().Add(10*time.Minute).Unix())
	t.Setenv("SHOULD_NOT_LEAK", "ambient-secret")
	output, err := execute(context.Background(), command[1], root, credentialPath, checkout)
	if err != nil || !strings.HasPrefix(string(output), "[redacted]|") || strings.Contains(string(output), "ambient-secret") || strings.Contains(string(output), "leased-secret") {
		t.Fatalf("unexpected CLI execution %q: %v", output, err)
	}
	slow := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.156.1'; exit; fi\nsleep 4\n")
	if err := os.WriteFile(binary, slow, 0700); err != nil {
		t.Fatal(err)
	}
	slowHash := sha256.Sum256(slow)
	request.Runtime.BinarySHA256 = hex.EncodeToString(slowHash[:])
	command, err = Command(request)
	if err != nil {
		t.Fatal(err)
	}
	writeCredential("attempt-1", "openai", time.Now().Add(2*time.Second).Unix())
	start := time.Now()
	if _, err := execute(context.Background(), command[1], root, credentialPath, checkout); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("tool continued past credential expiry: %v", err)
	}
	request.Profile.Effort = "medium"
	if _, err := Command(request); !errors.Is(err, ErrBlocked) {
		t.Fatalf("unapproved effort must block: %v", err)
	}
}
