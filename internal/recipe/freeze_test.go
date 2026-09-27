package recipe_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/mjtechguy/blaxsmith/internal/guild"
	. "github.com/mjtechguy/blaxsmith/internal/recipe"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/limit"
)

const example = "examples/guild/"

func TestFreezeOpenCodeProfile(t *testing.T) {
	in := testRepo(t)
	in.Recipe = example + "recipe-opencode.json"
	bundle, err := Freeze(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	profile := bundle.Recipe.Profiles["reviewer"]
	if profile.Harness != "opencode" || profile.Model != "example-provider/example-model" || profile.Effort != "provider-default" {
		t.Fatalf("OpenCode selection changed: %+v", profile)
	}
}

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
	if !strings.Contains(first.Validation.Report, "PASS") || strings.Contains(first.Validation.Report, "WARNING") || first.StageOrder[len(first.StageOrder)-1] != "human-review" {
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
	if got[example+"skills/evidence/SKILL.md"] == "" || !strings.Contains(got[(example+"spec.md")], "FR-001") {
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
		{"agent approving", func(t *testing.T, in *Input, r *Recipe) { r.Stages[5].Profile = "architect" }, "cannot have an agent"},
		{"cycle", func(t *testing.T, in *Input, r *Recipe) { r.Stages[0].DependsOn = []string{"implement"} }, "cycle"},
		{"unknown dependency", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].DependsOn = []string{"missing"} }, "invalid or duplicate dependency"},
		{"duplicate dependency", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].DependsOn = []string{"plan", "plan"} }, "invalid or duplicate dependency"},
		{"duplicate stage", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].ID = "plan" }, "duplicate stage"},
		{"negative corrections", func(t *testing.T, in *Input, r *Recipe) { r.Limits.MaxCorrectionCycles = -1 }, "limits require"},
		{"unbounded timeout", func(t *testing.T, in *Input, r *Recipe) { r.Limits.TimeoutSeconds = 0 }, "limits require"},
		{"unreviewed work", func(t *testing.T, in *Input, r *Recipe) { r.Stages[4].DependsOn = []string{"review"} }, "must precede final"},
		{"loop on plan", func(t *testing.T, in *Input, r *Recipe) {
			r.Stages[0].Loop = &Loop{With: "plan", Until: "pass", MaxCycles: 2}
		}, "review/verify stage"},
		{"unbounded loop", func(t *testing.T, in *Input, r *Recipe) { r.Stages[3].Loop.MaxCycles = 11 }, "1–10 cycles"},
		{"loop downstream", func(t *testing.T, in *Input, r *Recipe) { r.Stages[3].Loop.With = "architect-review" }, "upstream agent stage"},
		{"duplicate checks", func(t *testing.T, in *Input, r *Recipe) { r.RequiredChecks = []string{"tests", "tests"} }, "duplicate required check"},
		{"missing profile", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].Profile = "unknown" }, "known profile"},
		{"unsupported harness", func(t *testing.T, in *Input, r *Recipe) {
			p := r.Profiles["implementer"]
			p.Harness = "native-grok"
			r.Profiles["implementer"] = p
		}, "explicit Claude Code/Codex"},
		{"opencode missing provider", func(t *testing.T, in *Input, r *Recipe) {
			p := r.Profiles["implementer"]
			p.Harness, p.Model = "opencode", "unqualified-model"
			r.Profiles["implementer"] = p
		}, "provider/model"},
		{"path escape", func(t *testing.T, in *Input, r *Recipe) { r.Stages[1].Prompt = "../outside.md" }, "prompt file"},
		{"missing file", func(t *testing.T, in *Input, r *Recipe) { r.Validation.Inputs["transcript"] = "missing.md" }, "not committed"},
		{"scope is file", func(t *testing.T, in *Input, r *Recipe) { in.Scope = (example + "spec.md") }, "contains no committed files"},
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
			write(t, in.Repo, (example + "spec.md"), "version https://git-lfs.github.com/spec/v1\noid sha256:abc\nsize 3\n")
		}, "LFS pointer"},
		{"oversized input", func(t *testing.T, in *Input, r *Recipe) {
			write(t, in.Repo, (example + "spec.md"), strings.Repeat("x", maxArtifactBytes+1))
		}, "must contain"},
		{"forged locked quote", func(t *testing.T, in *Input, r *Recipe) {
			data, err := os.ReadFile(filepath.Join(in.Repo, (example + "spec.md")))
			if err != nil {
				t.Fatal(err)
			}
			write(t, in.Repo, (example + "spec.md"), strings.Replace(string(data), "Freeze recipe inputs at an exact Git commit.", "Invent a requirement the user never gave.", 1))
		}, "NOT_VERBATIM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := testRepo(t)
			data, err := os.ReadFile(filepath.Join(in.Repo, in.Recipe))
			if err != nil {
				t.Fatal(err)
			}
			r, _, fe := Validate(data)
			if fe != nil {
				t.Fatal(fe)
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
		if _, _, err := Validate([]byte(data)); err == nil {
			t.Fatalf("accepted ambiguous recipe: %s", data)
		}
	}
}

func TestInterviewStage(t *testing.T) {
	data, err := os.ReadFile("../../" + example + "recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	r, _, fe := Validate(data)
	if fe != nil {
		t.Fatal(fe)
	}
	if r.Stages[3].Loop == nil || r.Stages[3].Loop.With != "implement" {
		t.Fatalf("example verify loop missing: %+v", r.Stages[3])
	}
	r.Stages[0].Kind = "interview"
	encoded, _ := json.Marshal(r)
	if _, _, err := Validate(encoded); err != nil {
		t.Fatalf("interview did not satisfy planning: %v", err)
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
	return Input{Validators: map[string]Validator{"guild-forge": guild.ValidateInputs}, Repo: dir, Ref: "HEAD", Recipe: example + "recipe.json", Scope: "examples/guild"}
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

const maxArtifactBytes = MaxRecipeBytes
const maxBundleBytes = 16 << 20

func digest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }
func sortedKeys(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
