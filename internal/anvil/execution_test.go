package anvil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func TestExecutionPackets(t *testing.T) {
	p := Plan{Schema: PlanSchema, Title: "Saved searches", Summary: "Reuse the service", Requirements: []Requirement{{ID: "R1", Description: "Persistence", Sources: []string{"brief"}, Examples: []string{"Reload restores settings"}}, {ID: "R2", Description: "Validation", Sources: []string{"brief"}, Examples: []string{"Invalid input keeps settings"}}}, Phases: []Phase{{ID: "P1", Title: "Deliver", Outcome: "Working feature"}}, Tasks: []Task{
		{ID: "T2", Title: "Validate journey", Phase: "P1", Reason: "Protect behavior", DependsOn: []string{"T1"}, RequirementIDs: []string{"R2"}, Instructions: "Exercise invalid inputs through the UI", Acceptance: []string{"Prior settings preserved"}},
		{ID: "T1", Title: "Implement persistence", Phase: "P1", Reason: "Retain settings", RequirementIDs: []string{"R1"}, Instructions: "Extend the existing service", Acceptance: []string{"Reload restores settings"}},
	}}
	data, _ := json.Marshal(p)
	in := ExecutionInput{GoalID: "goal", GoalRevision: 3, PlanVersion: 2, PlanJSON: data, Context: []byte(`{"brief":"saved searches"}`), References: map[string]bool{"brief": true}, Profile: recipe.Profile{Harness: "codex", Model: "test-model", Effort: "medium", Instructions: []string{"rules.md"}, Skills: []string{"skills/testing/SKILL.md", "skills/testing/checks.md"}}, RuntimeSeconds: 900, Acceptance: "manual"}
	body, files, packets, err := ExecutionRecipe(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) != 2 || packets[0].ID != "T1" || packets[1].ID != "T2" {
		t.Fatalf("dependency order: %+v", packets)
	}
	var first TaskPacket
	if err = json.Unmarshal(packets[0].JSON, &first); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if len(first.Requirements) != 1 || first.Requirements[0].ID != "R1" || first.Task.Instructions != p.Tasks[1].Instructions || first.PlanVersion != 2 || first.GoalRevision != 3 || first.PlanSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("packet lost scope/provenance: %+v", first)
	}
	again, _, second, err := ExecutionRecipe(in)
	if err != nil || !bytes.Equal(body, again) || !bytes.Equal(packets[0].JSON, second[0].JSON) {
		t.Fatalf("non-deterministic compilation: %v", err)
	}
	r, _, fe := recipe.Validate(body)
	if fe != nil || len(r.Stages) != 2 || r.Stages[0].Kind != "implement" || r.Stages[1].Kind != "verify" || len(r.Documents) != 5 || len(r.Profiles["implementer"].Instructions) != 1 || r.Limits.MaxCorrectionCycles != 0 {
		t.Fatalf("wrong executable recipe: %+v %v", r, fe)
	}
	repo := t.TempDir()
	if err = os.WriteFile(filepath.Join(repo, "README.md"), []byte("Existing repository"), 0600); err != nil {
		t.Fatal(err)
	}
	contextFiles := map[string]string{"rules.md": "Keep the existing stack and API boundaries.", "AGENTS.md": "Use focused tests.", "skills/testing/SKILL.md": "---\nname: testing\ndescription: Test the selected behavior\n---\nRead checks.md before testing.", "skills/testing/checks.md": "Exercise the error path."}
	for name, data := range contextFiles {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"init", "--template=", "--initial-branch=main"}, {"add", "."}, {"commit", "-m", "fixture"}} {
		base := []string{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		if output, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	frozen, err := recipe.Freeze(t.Context(), recipe.Input{Repo: repo, Ref: "HEAD", Recipe: ExecutionRecipePath, RecipeData: body, PlatformFiles: files, Scope: "."})
	if err != nil {
		t.Fatal(err)
	}
	for _, packet := range packets {
		found := false
		for _, a := range frozen.Artifacts {
			if a.Path == packet.Path {
				found = bytes.Equal(a.Data, packet.JSON) && a.SHA256 == packet.SHA256 && strings.Contains(a.Source, "plan:2#")
			}
		}
		if !found {
			t.Fatalf("packet not frozen: %s", packet.ID)
		}
	}
	// Both native phases preserve the same explicit Git context. Platform task
	// packets stay separate from checkout-backed instructions and skill files.
	planning, planningFiles, err := PlanningRecipe(in.GoalID, in.GoalRevision, in.Context, in.Profile, in.RuntimeSeconds)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := recipe.Freeze(t.Context(), recipe.Input{Repo: repo, Ref: "HEAD", Recipe: PlanningRecipePath, RecipeData: planning, PlatformFiles: planningFiles, Scope: "."})
	if err != nil {
		t.Fatal(err)
	}
	for _, bundle := range []*recipe.Bundle{frozen, planned} {
		for name, data := range contextFiles {
			found := false
			for _, artifact := range bundle.Artifacts {
				if artifact.Path == name {
					found = artifact.Source == "" && string(artifact.Data) == data
				}
			}
			if !found {
				t.Fatalf("%s did not freeze repository context %s", bundle.Recipe.Name, name)
			}
		}
	}
	changed := in
	changed.Profile.Instructions = []string{"missing.md"}
	changedBody, changedFiles, _, err := ExecutionRecipe(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recipe.Freeze(t.Context(), recipe.Input{Repo: repo, Ref: "HEAD", Recipe: ExecutionRecipePath, RecipeData: changedBody, PlatformFiles: changedFiles, Scope: "."}); err == nil {
		t.Fatal("missing repository rule silently ignored")
	}
	changed.Profile.Instructions = nil
	changedBody, changedFiles, _, err = ExecutionRecipe(changed)
	if err != nil {
		t.Fatal(err)
	}
	without, err := recipe.Freeze(t.Context(), recipe.Input{Repo: repo, Ref: "HEAD", Recipe: ExecutionRecipePath, RecipeData: changedBody, PlatformFiles: changedFiles, Scope: "."})
	if err != nil || without.Digest == frozen.Digest {
		t.Fatalf("changed rules did not invalidate preview: %v", err)
	}
	for _, path := range []string{"../outside.md", "/etc/passwd"} {
		changed.Profile.Instructions = []string{path}
		if _, _, _, err := ExecutionRecipe(changed); err == nil {
			t.Fatalf("unsafe context path accepted: %s", path)
		}
	}
	in.Acceptance = "policy"
	in.CorrectionCycles = 2
	body, _, _, err = ExecutionRecipe(in)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _ = recipe.Validate(body)
	if r.Acceptance != "policy" || r.Limits.MaxCorrectionCycles != 2 {
		t.Fatal("user settings ignored")
	}
	if loop := r.Stages[1].Loop; loop == nil || loop.With != "implement" || loop.Until != "pass" || loop.MaxCycles != 2 {
		t.Fatalf("verification cannot request the selected corrections: %+v", loop)
	}
	in.CorrectionCycles = 0
	body, _, _, err = ExecutionRecipe(in)
	if err != nil {
		t.Fatal(err)
	}
	r, _, _ = recipe.Validate(body)
	if r.Stages[1].Loop != nil {
		t.Fatal("zero corrections enabled automatic repair")
	}
	in.PlanJSON = []byte(strings.Replace(string(data), `"depends_on":null`, `"depends_on":["T2"]`, 1))
	if _, _, _, err = ExecutionRecipe(in); err == nil {
		t.Fatal("cycle accepted")
	}
	in.PlanJSON = data
	in.Context = []byte(`{"text":"` + strings.Repeat("x", 256<<10) + `"}`)
	if _, _, _, err = ExecutionRecipe(in); err == nil {
		t.Fatal("context silently truncated")
	}
}
