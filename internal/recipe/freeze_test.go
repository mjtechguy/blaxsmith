package recipe

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/limit"
)

const example = "examples/guild/"

func TestFreezeCommittedGuildWorkflow(t *testing.T) {
	in := testRepo(t)
	write(t, in.Repo, "AGENTS.md", "Root instructions.\n")
	write(t, in.Repo, example+"prompts/AGENTS.md", "Prompt-directory instructions.\n")
	write(t, in.Repo, "unrelated/AGENTS.md", "Must not be included.\n")
	commit(t, in.Repo)
	first, err := Freeze(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Guild.Report, "PASS") || strings.Contains(first.Guild.Report, "WARNING") || first.StageOrder[len(first.StageOrder)-1] != "human-review" {
		t.Fatalf("invalid gate result: %+v", first)
	}
	got := map[string]string{}
	for _, artifact := range first.Artifacts {
		got[artifact.Path] = string(artifact.Data)
		if digest(artifact.Data) != artifact.SHA256 {
			t.Fatalf("artifact digest mismatch: %s", artifact.Path)
		}
	}
	if got["AGENTS.md"] == "" || got[example+"AGENTS.md"] == "" || got[example+"prompts/AGENTS.md"] == "" || got["unrelated/AGENTS.md"] != "" {
		t.Fatalf("incorrect AGENTS.md scope: %v", sortedKeys(got))
	}
	if got[example+"skills/evidence/SKILL.md"] == "" || !strings.Contains(got[in.Spec], "FR-001") {
		t.Fatal("missing skill or Guild requirement IDs")
	}
	// A dirty prompt and hostile caller Git environment must not change the source.
	write(t, in.Repo, example+"prompts/implement.md", "Changed instructions.\n")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-a-repository"))
	again, err := Freeze(t.Context(), in)
	if err != nil || again.Digest != first.Digest {
		t.Fatalf("dirty checkout changed frozen inputs: %v", err)
	}
	t.Setenv("GIT_DIR", filepath.Join(in.Repo, ".git"))
	commit(t, in.Repo)
	changed, err := Freeze(t.Context(), in)
	if err != nil || changed.Digest == first.Digest {
		t.Fatalf("new committed instructions did not change provenance: %v", err)
	}
	in.Ref = first.Source.Commit
	pinned, err := Freeze(t.Context(), in)
	if err != nil || pinned.Digest != first.Digest {
		t.Fatalf("old commit no longer reproducible: %v", err)
	}
	want := pinned.Digest
	pinned.Digest = ""
	canonical, err := json.Marshal(pinned)
	if err != nil || digest(canonical) != want {
		t.Fatalf("bundle digest cannot be independently recomputed: %v", err)
	}
	in.Repo = filepath.Join(in.Repo, example)
	fromSubdir, err := Freeze(t.Context(), in)
	if err != nil || fromSubdir.Digest != first.Digest {
		t.Fatalf("subdirectory changed repository-relative paths: %v", err)
	}
}

func TestRejectInvalidRunInputs(t *testing.T) {
	cases := []struct {
		name string
		edit func(*testing.T, *Input, *Recipe)
		want string
	}{
		{"missing human", func(t *testing.T, in *Input, r *Recipe) { r.Stages = r.Stages[:5] }, "missing mandatory human_review"},
		{"agent approving", func(t *testing.T, in *Input, r *Recipe) { r.Stages[5].Profile = "architect" }, "cannot have an agent"},
		{"cycle", func(t *testing.T, in *Input, r *Recipe) { r.Stages[0].DependsOn = []string{"implement"} }, "cycle"},
		{"unknown dependency", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].DependsOn = []string{"missing"} }, "invalid or duplicate dependency"},
		{"duplicate dependency", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].DependsOn = []string{"plan", "plan"} }, "invalid or duplicate dependency"},
		{"duplicate stage", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].ID = "plan" }, "duplicate stage"},
		{"unbounded corrections", func(t *testing.T, in *Input, r *Recipe) { r.Limits.MaxCorrectionCycles = 0 }, "limits require"},
		{"unbounded timeout", func(t *testing.T, in *Input, r *Recipe) { r.Limits.TimeoutSeconds = 0 }, "limits require"},
		{"unchecked implementation", func(t *testing.T, in *Input, r *Recipe) { r.Stages[3].DependsOn = []string{"plan"} }, "subsequent review and verification"},
		{"unreviewed work", func(t *testing.T, in *Input, r *Recipe) { r.Stages[4].DependsOn = []string{"review"} }, "must precede final"},
		{"post architect work", func(t *testing.T, in *Input, r *Recipe) {
			r.Stages = append(r.Stages, Stage{ID: "late", Kind: "documentation", Profile: "architect", Prompt: example + "prompts/plan.md", DependsOn: []string{"architect-review"}})
			r.Stages[5].DependsOn = []string{"late"}
		}, "must precede final architect"},
		{"missing policy checks", func(t *testing.T, in *Input, r *Recipe) { r.RequiredChecks = nil }, "require checks"},
		{"duplicate checks", func(t *testing.T, in *Input, r *Recipe) { r.RequiredChecks = []string{"tests", "tests"} }, "duplicate required check"},
		{"missing profile", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].Profile = "unknown" }, "known profile"},
		{"unsupported harness", func(t *testing.T, in *Input, r *Recipe) {
			p := r.Profiles["implementer"]
			p.Harness = "opencode"
			r.Profiles["implementer"] = p
		}, "explicit Claude Code/Codex"},
		{"path escape", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].Prompt = "../outside.md" }, "prompt file"},
		{"missing file", func(t *testing.T, in *Input, r *Recipe) { in.Transcript = "missing.md" }, "not committed"},
		{"scope is file", func(t *testing.T, in *Input, r *Recipe) { in.Scope = in.Spec }, "contains no committed files"},
		{"scope traversal", func(t *testing.T, in *Input, r *Recipe) { in.Scope = "../outside" }, "repository-relative"},
		{"symlink", func(t *testing.T, in *Input, r *Recipe) {
			name := filepath.Join(in.Repo, example+"AGENTS.md")
			if err := os.Remove(name); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("spec.md", name); err != nil {
				t.Fatal(err)
			}
		}, "regular Git blob"},
		{"LFS pointer", func(t *testing.T, in *Input, r *Recipe) {
			write(t, in.Repo, in.Spec, "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 3\n")
		}, "LFS pointer"},
		{"oversized input", func(t *testing.T, in *Input, r *Recipe) {
			write(t, in.Repo, in.Spec, strings.Repeat("x", maxArtifactBytes+1))
		}, "must contain"},
		{"forged locked quote", func(t *testing.T, in *Input, r *Recipe) {
			data, err := os.ReadFile(filepath.Join(in.Repo, in.Spec))
			if err != nil {
				t.Fatal(err)
			}
			write(t, in.Repo, in.Spec, strings.Replace(string(data), "Freeze recipe inputs at an exact Git commit.", "Invent a requirement the user never gave.", 1))
		}, "NOT_VERBATIM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testRepo(t)
			data, err := os.ReadFile(filepath.Join(in.Repo, in.Recipe))
			if err != nil {
				t.Fatal(err)
			}
			r, err := parse(data)
			if err != nil {
				t.Fatal(err)
			}
			tc.edit(t, &in, &r)
			data, err = json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			write(t, in.Repo, in.Recipe, string(data))
			commit(t, in.Repo)
			_, err = Freeze(t.Context(), in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("wanted %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRejectAmbiguousJSON(t *testing.T) {
	for _, data := range []string{
		`{"name":"a","name":"b"}`, `{"profiles":{"a":{},"A":{}}}`,
		`{"unknown":true}`, `{} {}`, `{"limits":{"timeout_seconds":"oops"}}`,
	} {
		if _, err := parse([]byte(data)); err == nil {
			t.Fatalf("accepted ambiguous recipe: %s", data)
		}
	}
}

func TestBoundedGitOutput(t *testing.T) {
	output := limit.Buffer{Max: maxBundleBytes}
	_, err := io.Copy(&output, strings.NewReader(strings.Repeat("x", maxBundleBytes+1)))
	if err == nil || len(output.Bytes()) > maxBundleBytes {
		t.Fatal("subprocess output limit was bypassed")
	}
}

func testRepo(t *testing.T) Input {
	t.Helper()
	dir := t.TempDir()
	err := fs.WalkDir(os.DirFS("../../examples/guild"), ".", func(name string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(filepath.Join("../../examples/guild", name))
		if err != nil {
			return err
		}
		write(t, dir, example+name, string(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "init", "--template=", "--initial-branch=main")
	commit(t, dir)
	return Input{Repo: dir, Ref: "HEAD", Recipe: example + "recipe.json", Spec: example + "spec.md", Transcript: example + "transcript.md", Scope: "examples/guild"}
}

func write(t *testing.T, repo, name, data string) {
	t.Helper()
	file := filepath.Join(repo, name)
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, repo string) {
	t.Helper()
	testGit(t, repo, "add", ".")
	testGit(t, repo, "commit", "--allow-empty", "-m", "test inputs")
}

func testGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	base := []string{"-C", repo, "-c", "user.name=Blaxsmith Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
	cmd := exec.CommandContext(context.Background(), "git", append(base, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
