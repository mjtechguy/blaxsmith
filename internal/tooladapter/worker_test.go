package tooladapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
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

func TestAXWorkspaceSourceMustMatchFrozenRepositoryAndCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for workspace source verification")
	}
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", source}, args...)...)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		} else {
			return output
		}
		return nil
	}
	run("init", "--quiet")
	run("config", "user.name", "Workspace test")
	run("config", "user.email", "workspace@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("pinned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "--quiet", "-m", "pinned source")
	run("config", "remote.origin.url", "https://github.com/owner/repo")
	commit := strings.TrimSpace(string(run("rev-parse", "HEAD^{commit}")))
	path, err := workspaceSourcePath(root, "source")
	if err != nil || verifyWorkspaceCheckout(t.Context(), path, "https://github.com/owner/repo", commit) != nil {
		t.Fatalf("valid AX workspace source rejected: %q %v", path, err)
	}
	if verifyWorkspaceCheckout(t.Context(), path, "https://github.com/owner/other", commit) == nil ||
		verifyWorkspaceCheckout(t.Context(), path, "https://github.com/owner/repo", strings.Repeat("0", 40)) == nil {
		t.Fatal("changed repository or commit accepted")
	}
	if _, err := workspaceSourcePath(root, "../source"); err == nil {
		t.Fatal("workspace path traversal accepted")
	}
	if err := os.Symlink(source, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	if _, err := workspaceSourcePath(root, "linked"); err == nil {
		t.Fatal("workspace symlink accepted")
	}
	binary := filepath.Join(root, "codex")
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.156.1'; else printf '%s' \"$PWD\"; fi\n")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	request := Request{AttemptID: "attempt-a", RepositoryURL: "https://github.com/owner/repo",
		SourceCommit: commit, SourceDirectory: "source", Runtime: Runtime{Harness: "codex",
			Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
			BinarySHA256: hex.EncodeToString(hash[:]), Version: "0.156.1",
			Supported: []ModelEffort{{Model: "gpt-6-luna", Effort: "xhigh"}}},
		Profile: recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"},
		Prompt:  "Use the AX workspace", TimeoutSeconds: 60, MaxOutputBytes: 1024}
	command, err := Command(request)
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(root, "credential.json")
	credentialData, _ := json.Marshal(credential{AttemptID: "attempt-a", Provider: "openai",
		ExpiresAt: time.Now().Add(time.Minute).Unix(), APIKey: "leased-key"})
	if err := os.WriteFile(credentialPath, credentialData, 0600); err != nil {
		t.Fatal(err)
	}
	checkout := func(context.Context, string, string, string, string) error { return errors.New("unexpected checkout") }
	output, err := execute(t.Context(), command[1], root, credentialPath, checkout)
	if err != nil || string(output) != path {
		t.Fatalf("worker did not run in the verified AX source directory: %q %v", output, err)
	}
}

func TestWorkerChecksFrozenSkillAgainstPinnedCheckout(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "codex")
	program := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'codex-cli 0.156.1'; elif [ ! -f \"$HOME/.agents/skills/evidence/SKILL.md\" ] || [ ! -f \"$HOME/.agents/skills/evidence/references/evidence.md\" ]; then exit 9; else printf ran; fi\n")
	if err := os.WriteFile(binary, program, 0700); err != nil {
		t.Fatal(err)
	}
	programSHA := sha256.Sum256(program)
	skill := "skills/evidence/SKILL.md"
	if err := os.MkdirAll(filepath.Join(root, "skills/evidence"), 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("---\nname: evidence\ndescription: Keep findings tied to frozen requirements and source evidence.\n---\n\nReview the evidence.")
	reference := "skills/evidence/references/evidence.md"
	referenceContent := []byte("Evidence means a requirement, revision, and observed result.")
	if err := os.MkdirAll(filepath.Join(root, "skills/evidence/references"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, skill), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, reference), referenceContent, 0600); err != nil {
		t.Fatal(err)
	}
	contentSHA := sha256.Sum256(content)
	referenceSHA := sha256.Sum256(referenceContent)
	request := Request{AttemptID: "attempt-1", RepositoryURL: "https://github.com/example/repo", SourceRef: "main",
		SourceCommit: strings.Repeat("a", 40), Runtime: Runtime{Harness: "codex",
			Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
			BinarySHA256: hex.EncodeToString(programSHA[:]), Version: "0.156.1",
			Supported: []ModelEffort{{Model: "gpt-6-luna", Effort: "xhigh"}}},
		Profile: recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh", Skills: []string{skill, reference}},
		Prompt:  "Frozen native skill: evidence", FrozenArtifacts: []ArtifactDigest{{Path: skill, SHA256: hex.EncodeToString(contentSHA[:])}, {Path: reference, SHA256: hex.EncodeToString(referenceSHA[:])}},
		TimeoutSeconds: 60, MaxOutputBytes: 1024}
	command, err := Command(request)
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(root, "credential.json")
	body, _ := json.Marshal(credential{AttemptID: request.AttemptID, Provider: "openai", ExpiresAt: time.Now().Add(10 * time.Minute).Unix(), APIKey: "leased"})
	if err := os.WriteFile(credentialPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	checkout := func(context.Context, string, string, string, string) error { return nil }
	if output, err := execute(t.Context(), command[1], root, credentialPath, checkout); err != nil || string(output) != "ran" {
		t.Fatalf("verified skill did not run: %q %v", output, err)
	}
	if err := os.WriteFile(filepath.Join(root, skill), []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t.Context(), command[1], root, credentialPath, checkout); !errors.Is(err, ErrBlocked) {
		t.Fatalf("changed skill must block: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, skill), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("ambient"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := execute(t.Context(), command[1], root, credentialPath, checkout); !errors.Is(err, ErrBlocked) {
		t.Fatalf("unlisted project instructions must block: %v", err)
	}
	request.FrozenArtifacts = nil
	if _, err := Command(request); !errors.Is(err, ErrBlocked) {
		t.Fatalf("unfrozen skill must block: %v", err)
	}
	request.FrozenArtifacts = []ArtifactDigest{{Path: skill, SHA256: hex.EncodeToString(contentSHA[:])}, {Path: reference, SHA256: hex.EncodeToString(referenceSHA[:])}}
	request.Prompt = strings.Repeat("x", maxTaskArg)
	if _, err := Command(request); !errors.Is(err, ErrBlocked) {
		t.Fatalf("oversized AX command argument must block: %v", err)
	}
}

func TestPortableSkillMaterializesInEachHarnessAndRejectsToolGrants(t *testing.T) {
	workdir := t.TempDir()
	skill := "skills/evidence/SKILL.md"
	content := []byte("---\nname: evidence\ndescription: Keep findings tied to frozen requirements and source evidence.\nmetadata:\n  owner: platform\n---\n\nReview the evidence.")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(workdir, skill)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, skill), content, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	artifacts := []ArtifactDigest{{Path: skill, SHA256: hex.EncodeToString(sum[:])}}
	for _, tc := range []struct{ harness, file, claudeDir string }{
		{"codex", ".agents/skills/evidence/SKILL.md", ""},
		{"claude-code", "skill-source/.claude/skills/evidence/SKILL.md", "skill-source"},
		{"opencode", ".config/opencode/skills/evidence/SKILL.md", ""},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			home := t.TempDir()
			claudeDir, err := materializeSkills(workdir, home, tc.harness, []string{skill}, artifacts)
			if err != nil || (tc.claudeDir != "" && claudeDir != filepath.Join(home, tc.claudeDir)) {
				t.Fatalf("materialize %s skill: %q %v", tc.harness, claudeDir, err)
			}
			installed := filepath.Join(home, filepath.FromSlash(tc.file))
			got, err := os.ReadFile(installed)
			info, statErr := os.Stat(installed)
			if err != nil || statErr != nil || string(got) != string(content) || info.Mode().Perm() != 0400 {
				t.Fatalf("skill was not installed read-only: %q %v %v", got, err, statErr)
			}
		})
	}
	unsafe := []byte("---\nname: evidence\ndescription: Evidence helper.\nallowed-tools: Bash(*)\n---\nGrant tools")
	if err := os.WriteFile(filepath.Join(workdir, skill), unsafe, 0600); err != nil {
		t.Fatal(err)
	}
	unsafeSHA := sha256.Sum256(unsafe)
	if _, err := materializeSkills(workdir, t.TempDir(), "claude-code", []string{skill}, []ArtifactDigest{{Path: skill, SHA256: hex.EncodeToString(unsafeSHA[:])}}); !errors.Is(err, ErrBlocked) {
		t.Fatalf("skill frontmatter must not grant tools: %v", err)
	}
}

func TestClaudeBareModeGetsOnlyDeclaredSkillDirectory(t *testing.T) {
	workdir := t.TempDir()
	skill := "skills/evidence/SKILL.md"
	content := []byte("---\nname: evidence\ndescription: Review evidence.\n---\n\nReview evidence.")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(workdir, skill)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, skill), content, 0600); err != nil {
		t.Fatal(err)
	}
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.280 (Claude Code)'; exit; fi\nfound=0\nprevious=\nfor arg in \"$@\"; do if [ \"$previous\" = \"--add-dir\" ] && [ \"$arg\" = \"$HOME/skill-source\" ]; then found=1; fi; previous=\"$arg\"; done\ntest \"$found\" = 1 && test -f \"$HOME/skill-source/.claude/skills/evidence/SKILL.md\" || exit 9\nprintf ok\n")
	binary := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	binarySHA := sha256.Sum256(body)
	skillSHA := sha256.Sum256(content)
	runtime := Runtime{Harness: "claude-code", Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
		BinarySHA256: hex.EncodeToString(binarySHA[:]), Version: "2.1.280", Supported: []ModelEffort{{Model: "claude-opus-5-5", Effort: "high"}}}
	in, err := Prepare(runtime, recipe.Profile{Harness: "claude-code", Model: "claude-opus-5-5", Effort: "high", Skills: []string{skill}},
		"Use the selected skill", time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	in.skillArtifacts = []ArtifactDigest{{Path: skill, SHA256: hex.EncodeToString(skillSHA[:])}}
	if output, err := Run(t.Context(), in, workdir, nil); err != nil || string(output) != "ok" {
		t.Fatalf("Claude did not receive the scoped skill directory: %q %v", output, err)
	}
}

func TestOpenCodeReceivesSelectedSkillWithoutNativeDelegation(t *testing.T) {
	workdir := t.TempDir()
	skill := "skills/evidence/SKILL.md"
	content := []byte("---\nname: evidence\ndescription: Review evidence.\n---\n\nReview evidence.")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(workdir, skill)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workdir, skill), content, 0600); err != nil {
		t.Fatal(err)
	}
	body := []byte("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'opencode v2.1.280'; exit; fi\nconfig=\"$XDG_CONFIG_HOME/opencode/opencode.json\"\ntest -f \"$HOME/.config/opencode/skills/evidence/SKILL.md\" || exit 9\ngrep -q '\"task\":\"deny\"' \"$config\" || exit 10\ngrep -q '\"websearch\":\"deny\"' \"$config\" || exit 11\ngrep -q '\"skill\":\"allow\"' \"$config\" || exit 12\nprintf ok\n")
	binary := filepath.Join(t.TempDir(), "opencode")
	if err := os.WriteFile(binary, body, 0700); err != nil {
		t.Fatal(err)
	}
	binarySHA := sha256.Sum256(body)
	skillSHA := sha256.Sum256(content)
	runtime := Runtime{Harness: "opencode", Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
		BinarySHA256: hex.EncodeToString(binarySHA[:]), Version: "2.1.280", Supported: []ModelEffort{{Model: "openai/gpt-6-luna", Effort: "high"}}}
	in, err := Prepare(runtime, recipe.Profile{Harness: "opencode", Model: "openai/gpt-6-luna", Effort: "high", Skills: []string{skill}},
		"Use the selected skill", time.Minute, 1024)
	if err != nil {
		t.Fatal(err)
	}
	in.skillArtifacts = []ArtifactDigest{{Path: skill, SHA256: hex.EncodeToString(skillSHA[:])}}
	if output, err := Run(t.Context(), in, workdir, nil); err != nil || string(output) != "ok" {
		t.Fatalf("OpenCode did not receive scoped skill configuration: %q %v", output, err)
	}
}
