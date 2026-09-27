package anvil

import (
	"encoding/json"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"testing"
)

func TestPlanContract(t *testing.T) {
	valid := Plan{Schema: PlanSchema, Title: "Saved search", Summary: "Reuse the search service", Requirements: []Requirement{{ID: "R1", Description: "Save a search", Sources: []string{"brief"}, Examples: []string{"Saved search survives reload"}}}, Phases: []Phase{{ID: "P1", Title: "Deliver", Outcome: "Saved search works"}}, Tasks: []Task{{ID: "T1", Title: "Persist search", Phase: "P1", Reason: "Survive reload", RequirementIDs: []string{"R1"}, Instructions: "Extend the existing service", Acceptance: []string{"Reload restores search"}}}}
	for _, tc := range []struct {
		name   string
		change func(*Plan)
		ok     bool
	}{
		{"minimal optional checks", func(*Plan) {}, true},
		{"invented source", func(p *Plan) { p.Requirements[0].Sources = []string{"question:unanswered"} }, false},
		{"cycle", func(p *Plan) { p.Tasks[0].DependsOn = []string{"T1"} }, false},
		{"missing dependency", func(p *Plan) { p.Tasks[0].DependsOn = []string{"missing"} }, false},
		{"uncovered requirement", func(p *Plan) {
			p.Requirements = append(p.Requirements, Requirement{ID: "R2", Description: "Other", Sources: []string{"brief"}, Examples: []string{"Example"}})
		}, false},
		{"missing instructions", func(p *Plan) { p.Tasks[0].Instructions = "" }, false},
		{"unknown phase", func(p *Plan) { p.Tasks[0].Phase = "missing" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := json.Marshal(valid)
			var p Plan
			json.Unmarshal(data, &p)
			tc.change(&p)
			data, _ = json.Marshal(p)
			_, canonical, err := ValidatePlan(data, map[string]bool{"brief": true})
			if (err == nil) != tc.ok {
				t.Fatalf("valid=%v: %v", tc.ok, err)
			}
			if tc.ok {
				if _, _, err = ValidatePlan(canonical, map[string]bool{"brief": true}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	data, _ := json.Marshal(valid)
	data = append(data[:len(data)-1], []byte(`,"execute":true}`)...)
	if _, _, err := ValidatePlan(data, map[string]bool{"brief": true}); err == nil {
		t.Fatal("unknown execution field accepted")
	}
	data, files, err := PlanningRecipe("goal", 1, []byte(`{"goal":"brief"}`), recipe.Profile{Harness: "codex", Model: "test-model", Effort: "medium"}, 900)
	if err != nil {
		t.Fatal(err)
	}
	r, order, fieldError := recipe.Validate(data)
	if fieldError != nil || len(order) != 2 || r.Stages[0].Kind != "plan" || r.Stages[1].Kind != "verify" || r.Acceptance != "manual" || r.Limits.MaxCorrectionCycles != 0 || r.Limits.MaxRuntimeSeconds != 900 || len(files) != 3 {
		t.Fatalf("unbounded planning recipe: %+v %v", r, fieldError)
	}
}
