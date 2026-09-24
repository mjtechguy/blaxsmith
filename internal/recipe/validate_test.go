package recipe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateReportsFieldPaths(t *testing.T) {
	data, err := os.ReadFile("../../" + example + "recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, order, fe := Validate(data); fe != nil || order[len(order)-1] != "human-review" {
		t.Fatalf("example rejected: %v %v", fe, order)
	}
	cases := []struct{ from, to, path, message string }{
		{`"max_cycles": 3}`, `"max_cycles": 11}`, "stages[3].loop", "1–10 cycles"},
		{`"profile": "implementer"`, `"profile": "nobody"`, "stages[1].profile", "known profile"},
		{`"depends_on": ["plan"]`, `"depends_on": ["missing"]`, "stages[1].depends_on[0]", "invalid or duplicate dependency"},
		{`"timeout_seconds": 1800`, `"timeout_seconds": "slow"`, "limits.timeout_seconds", "expected int"},
		{`"required_checks": [`, `"required_checks": [,`, "$", "line 31, column 24"},
		{`"name": "guild-engineering"`, `"name": "guild-engineering", "extra": 1`, "$", "unknown field"},
	}
	for _, tc := range cases {
		edited := strings.Replace(string(data), tc.from, tc.to, 1)
		if edited == string(data) {
			t.Fatalf("fixture edit %q did not apply", tc.from)
		}
		_, _, fe := Validate([]byte(edited))
		if fe == nil || fe.Path != tc.path || !strings.Contains(fe.Message, tc.message) {
			t.Fatalf("%s: got %+v, want %s / %s", tc.to, fe, tc.path, tc.message)
		}
	}
	if _, _, fe := Validate(nil); fe == nil || fe.Path != "$" {
		t.Fatalf("empty recipe accepted: %v", fe)
	}
}

func TestFreezeLibraryRecipeMatchesCommittedFile(t *testing.T) {
	in := testRepo(t)
	fromFile, err := Freeze(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(in.Repo, in.Recipe))
	if err != nil {
		t.Fatal(err)
	}
	library := in
	library.RecipeData = data
	fromLibrary, err := Freeze(t.Context(), library)
	if err != nil || fromLibrary.Digest != fromFile.Digest {
		t.Fatalf("library freeze changed provenance: %v", err)
	}
	// A library recipe needs no committed file at its path label.
	library.Recipe = ".blaxsmith/recipes/guild-engineering.json"
	labelled, err := Freeze(t.Context(), library)
	if err != nil || labelled.Source.Recipe != library.Recipe || labelled.Digest == fromFile.Digest {
		t.Fatalf("library label not recorded: %v", err)
	}
	library.Recipe = in.Spec
	if _, err := Freeze(t.Context(), library); err == nil || !strings.Contains(err.Error(), "collides") {
		t.Fatalf("label collision accepted: %v", err)
	}
}
