package recipe_test

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/mjtechguy/blaxsmith/internal/recipe"
)

func TestFactoryNeutralFreeze(t *testing.T) {
	in := testRepo(t)
	in.Validators = nil
	r := Recipe{SchemaVersion: Schema, Name: "native", Acceptance: "policy",
		Profiles: map[string]Profile{"worker": {Harness: "codex", Model: "example-model", Effort: "high"}},
		Stages:   []Stage{{ID: "work", Kind: "implement", Profile: "worker", Prompt: example + "prompts/implement.md"}},
		Limits:   Limits{MaxCorrectionCycles: 1, TimeoutSeconds: 60}, Documents: []string{example + "spec.md"}}
	in.RecipeData, _ = json.Marshal(r)
	b, err := Freeze(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if b.Validation != nil || len(b.StageOrder) != 1 {
		t.Fatalf("factory policy leaked into native run: %+v", b)
	}
	encoded, _ := json.Marshal(b)
	if strings.Contains(string(encoded), `"guild":`) {
		t.Fatal("factory-specific bundle contract")
	}
	r.Validation = &Validation{ID: "uninstalled", Inputs: map[string]string{"document": example + "spec.md"}}
	in.RecipeData, _ = json.Marshal(r)
	if _, err = Freeze(t.Context(), in); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("unknown validator did not fail closed: %v", err)
	}
	r.Validation = nil
	r.Stages[0].DependsOn = []string{"work"}
	in.RecipeData, _ = json.Marshal(r)
	if _, err = Freeze(t.Context(), in); err == nil {
		t.Fatal("neutral graph bypassed dependency validation")
	}
}

func TestPlatformInputsFreeze(t *testing.T) {
	in := testRepo(t)
	const name = ".blaxsmith/platform/context.md"
	r := Recipe{SchemaVersion: Schema, Name: "planner", Acceptance: "manual", Profiles: map[string]Profile{"p": {Harness: "codex", Model: "example-model", Effort: "medium"}}, Stages: []Stage{{ID: "plan", Kind: "plan", Profile: "p", Prompt: name}}, Limits: Limits{TimeoutSeconds: 60}}
	in.RecipeData, _ = json.Marshal(r)
	in.PlatformFiles = map[string]PlatformFile{name: {Data: []byte("saved goal"), Source: "goal:one@1"}}
	b, err := Freeze(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Freeze(t.Context(), in)
	if err != nil || again.Digest != b.Digest {
		t.Fatalf("unstable freeze: %v", err)
	}
	found := false
	for _, a := range b.Artifacts {
		if a.Path == name {
			found = string(a.Data) == "saved goal" && a.Source == "goal:one@1"
		}
	}
	if !found {
		t.Fatal("platform input missing provenance")
	}
	in.PlatformFiles[name] = PlatformFile{Data: []byte("new goal"), Source: "goal:one@2"}
	again, err = Freeze(t.Context(), in)
	if err != nil || again.Digest == b.Digest {
		t.Fatalf("goal changed without digest change: %v", err)
	}
	in.PlatformFiles[".blaxsmith/platform/unused"] = PlatformFile{Data: []byte("unused"), Source: "goal:one@2"}
	if _, err = Freeze(t.Context(), in); err == nil {
		t.Fatal("unused platform input accepted")
	}
	delete(in.PlatformFiles, ".blaxsmith/platform/unused")
	write(t, in.Repo, name, "repo collision")
	commit(t, in.Repo)
	if _, err = Freeze(t.Context(), in); err == nil {
		t.Fatal("repository shadowed platform input")
	}
}
