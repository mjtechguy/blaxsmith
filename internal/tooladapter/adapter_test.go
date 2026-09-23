package tooladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func approved(harness, model, level string) Runtime {
	return Runtime{Harness: harness, Image: "registry.example/agent@sha256:" + strings.Repeat("a", 64),
		Binary: "/opt/blaxsmith/bin/" + harness, BinarySHA256: strings.Repeat("b", 64), Version: "2.1.280",
		Supported: []ModelEffort{{model, level}}}
}

func TestPreparePinsEveryToolAndBlocksUnsupportedSelections(t *testing.T) {
	tests := []struct {
		harness, model, level string
		args                  []string
	}{
		{"codex", "gpt-6-luna", "xhigh", []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--sandbox", "workspace-write", "--model", "gpt-6-luna", "--config", `approval_policy="never"`, "--config", `model_reasoning_effort="xhigh"`, "Do the work"}},
		{"claude-code", "claude-opus-5-5", "high", []string{"--bare", "--print", "--output-format", "stream-json", "--permission-prompts", "none", "--no-session-persistence", "--settings", `{"availableModels":["claude-opus-5-5"],"fallbackModel":[]}`, "--model", "claude-opus-5-5", "--effort", "high", "Do the work"}},
		{"opencode", "openai/gpt-6-luna", "high", []string{"run", "--standalone", "--format", "json", "--model", "openai/gpt-6-luna#high", "Do the work"}},
	}
	for _, tc := range tests {
		t.Run(tc.harness, func(t *testing.T) {
			runtime := approved(tc.harness, tc.model, tc.level)
			profile := recipe.Profile{Harness: tc.harness, Model: tc.model, Effort: tc.level}
			in, err := Prepare(runtime, profile, "Do the work", 30*time.Minute, 4096)
			if err != nil || in.Image() != runtime.Image || !reflect.DeepEqual(in.args, tc.args) {
				t.Fatalf("unexpected invocation: %+v, %v", in, err)
			}
			changed := profile
			changed.Effort = "medium"
			if _, err := Prepare(runtime, changed, "Do the work", time.Minute, 4096); !errors.Is(err, ErrBlocked) {
				t.Fatalf("unsupported effort must block: %v", err)
			}
			changed = profile
			changed.Skills = []string{"skills/evidence/SKILL.md"}
			if _, err := Prepare(runtime, changed, "Do the work", time.Minute, 4096); !errors.Is(err, ErrBlocked) {
				t.Fatalf("unloaded skill must block: %v", err)
			}
		})
	}
	if _, err := Prepare(approved("opencode", "openai/model", "provider-default"), recipe.Profile{Harness: "opencode", Model: "openai/model", Effort: "provider-default"}, "prompt", time.Minute, 1); !errors.Is(err, ErrBlocked) {
		t.Fatalf("implicit OpenCode variant must block: %v", err)
	}
	if _, err := Prepare(approved("claude-code", "opus", "high"), recipe.Profile{Harness: "claude-code", Model: "opus", Effort: "high"}, "prompt", time.Minute, 1); !errors.Is(err, ErrBlocked) {
		t.Fatalf("moving Claude alias must block: %v", err)
	}
}

func TestRunChecksBinaryVersionEnvironmentAndBounds(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	write := func(body string) string {
		t.Helper()
		data := []byte("#!/bin/sh\n" + body)
		if err := os.WriteFile(path, data, 0700); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		return hex.EncodeToString(sum[:])
	}
	makeInvocation := func(hash string, max int, timeout time.Duration) Invocation {
		t.Helper()
		runtime := approved("codex", "gpt-6-luna", "xhigh")
		runtime.Binary, runtime.BinarySHA256, runtime.Version = path, hash, "0.156.1"
		in, err := Prepare(runtime, recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"}, "Do the work", timeout, max)
		if err != nil {
			t.Fatal(err)
		}
		return in
	}
	base := `if [ "$1" = "--version" ]; then echo "codex-cli 0.156.1"; exit; fi
printf '%s|%s|%s' "$DISABLE_UPDATES" "$OPENAI_API_KEY" "${INHERITED_SHOULD_NOT_LEAK:-}"`
	hash := write(base)
	t.Setenv("INHERITED_SHOULD_NOT_LEAK", "secret")
	in := makeInvocation(hash, 1024, time.Minute)
	if _, err := Run(t.Context(), in, dir, []string{"LD_PRELOAD=bad"}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("unapproved environment must block: %v", err)
	}
	out, err := Run(t.Context(), in, dir, []string{"OPENAI_API_KEY=leased"})
	if err != nil || string(out) != "1|leased|" {
		t.Fatalf("unexpected bounded output %q: %v", out, err)
	}
	if _, err := Run(t.Context(), makeInvocation(strings.Repeat("c", 64), 1024, time.Minute), dir, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("binary mismatch must block: %v", err)
	}
	config := filepath.Join(dir, ".codex")
	if err := os.Mkdir(config, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), in, dir, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("project CLI config must block: %v", err)
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	wrongVersion := write(`if [ "$1" = "--version" ]; then echo "codex-cli 0.156.10"; exit; fi`)
	if _, err := Run(t.Context(), makeInvocation(wrongVersion, 1024, time.Minute), dir, nil); !errors.Is(err, ErrBlocked) {
		t.Fatalf("version substring must not match: %v", err)
	}
	tooMuch := write(`if [ "$1" = "--version" ]; then echo "codex-cli 0.156.1"; exit; fi
printf 123456789`)
	if _, err := Run(t.Context(), makeInvocation(tooMuch, 4, time.Minute), dir, nil); err == nil {
		t.Fatal("output overflow must fail")
	}
	slow := write(`if [ "$1" = "--version" ]; then echo "codex-cli 0.156.1"; exit; fi
sleep 3`)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := Run(ctx, makeInvocation(slow, 1024, time.Second), dir, nil); err == nil {
		t.Fatal("timeout must fail")
	}
}
