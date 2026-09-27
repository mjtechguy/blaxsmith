package repoinspect_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/repoinspect"
)

// fixture reads a testdata repository the way InspectRepository does: every
// file name plus the repoinspect.Wanted root files' contents.
func fixture(t *testing.T, name string) ([]string, map[string][]byte) {
	t.Helper()
	root := filepath.Join("testdata", name)
	var files []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			files = append(files, filepath.ToSlash(rel))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	contents := map[string][]byte{}
	for _, want := range repoinspect.Wanted {
		if data, err := os.ReadFile(filepath.Join(root, want)); err == nil {
			contents[want] = data
		}
	}
	return files, contents
}

func argv(cs []repoinspect.Command) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.ID + "=" + strings.Join(c.Command, " ")
	}
	return out
}

func TestInspectFixtureRepositories(t *testing.T) {
	cases := []struct {
		fixture, source, recipe string
		verification, setup     []string
		fileError               bool
	}{
		{"node-pnpm", "detected", "", []string{"test=pnpm run test", "lint=pnpm run lint", "typecheck=pnpm run typecheck"},
			[]string{"install=pnpm install --frozen-lockfile"}, false},
		{"node-npm-default", "detected", "", []string{}, []string{"install=npm ci"}, false},
		{"go-make", "detected", "", []string{"go-test=go test ./...", "go-vet=go vet ./...", "make-test=make test", "make-lint=make lint"},
			[]string{"go-mod-download=go mod download"}, false},
		{"python-uv", "detected", "", []string{"pytest=uv run pytest", "ruff=uv run ruff check ."}, []string{"uv-sync=uv sync"}, false},
		{"rust", "detected", "", []string{"cargo-test=cargo test"}, []string{"cargo-fetch=cargo fetch"}, false},
		{"project-file", "file", "Guild engineering", []string{"unit=make unit", "e2e=npm run e2e"}, []string{"deps=make deps"}, false},
		{"project-file-invalid", "detected", "", []string{"go-test=go test ./...", "go-vet=go vet ./..."},
			[]string{"go-mod-download=go mod download"}, true},
		{"empty", "none", "", []string{}, []string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			r := repoinspect.Inspect(fixture(t, tc.fixture))
			if r.Source != tc.source || r.Recipe != tc.recipe || (r.FileError != "") != tc.fileError ||
				!slices.Equal(argv(r.Verification), tc.verification) || !slices.Equal(argv(r.Setup), tc.setup) {
				t.Fatalf("got %+v\nverification %q\nsetup %q", r, argv(r.Verification), argv(r.Setup))
			}
			if r.Source != "none" && len(r.Evidence) == 0 {
				t.Fatal("no evidence")
			}
		})
	}
}

func TestParseProjectFileRejects(t *testing.T) {
	for name, body := range map[string]string{
		"unknown field":   `{"version":1,"checks":[]}`,
		"wrong version":   `{"version":2}`,
		"missing version": `{}`,
		"duplicate id":    `{"version":1,"verification":[{"id":"a","command":["x"]},{"id":"a","command":["y"]}]}`,
		"bad id":          `{"version":1,"verification":[{"id":"Unit Tests","command":["x"]}]}`,
		"empty argv":      `{"version":1,"setup":[{"id":"a","command":[]}]}`,
		"empty argument":  `{"version":1,"setup":[{"id":"a","command":["make",""]}]}`,
		"trailing data":   `{"version":1} {}`,
		"blank recipe":    `{"version":1,"recipe":"   "}`,
	} {
		if _, err := repoinspect.ParseProjectFile([]byte(body)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := repoinspect.ParseProjectFile([]byte(`{"$schema":"x","version":1,"recipe":"Guild engineering"}`)); err != nil {
		t.Fatal(err)
	}
}

// The schema documents exactly the fields repoinspect.ParseProjectFile accepts.
func TestSchemaMatchesParser(t *testing.T) {
	data, err := os.ReadFile("../../docs/project-file.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range schema.Properties {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var fields []string
	for i := range reflect.TypeFor[repoinspect.ProjectFile]().NumField() {
		tag, _, _ := strings.Cut(reflect.TypeFor[repoinspect.ProjectFile]().Field(i).Tag.Get("json"), ",")
		fields = append(fields, tag)
	}
	slices.Sort(fields)
	if !slices.Equal(keys, fields) || !slices.Equal(schema.Required, []string{"version"}) {
		t.Fatalf("schema %v, parser %v", keys, fields)
	}
}

// End to end through Git: a committed fixture read at a pinned commit.
func TestInspectCommittedRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS(filepath.Join("testdata", "node-pnpm"))); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "fixture"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	commit, files, contents, err := recipe.ReadFiles(t.Context(), repo, "HEAD", repoinspect.Wanted)
	if err != nil || len(commit) != 40 {
		t.Fatalf("read %q %v", commit, err)
	}
	if r := repoinspect.Inspect(files, contents); r.Source != "detected" || len(r.Verification) != 3 || r.Setup[0].Command[0] != "pnpm" {
		t.Fatalf("committed inspect %+v", r)
	}
}
