package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func TestDecodeFrozenTask(t *testing.T) {
	verification := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1",
		Checks: []VerificationCheck{{ID: "checks", Command: []string{"true"}}}}
	policyJSON, err := validateVerification(verification, []string{"checks"})
	if err != nil {
		t.Fatal(err)
	}
	policySHA := sha(policyJSON)
	bundle := recipe.Bundle{SchemaVersion: "blaxsmith.bundle/v1alpha1",
		Source: recipe.Source{Commit: strings.Repeat("a", 40)},
		Recipe: recipe.Recipe{Profiles: map[string]recipe.Profile{"architect": {Harness: "codex", Model: "model", Effort: "high"}},
			Stages:         []recipe.Stage{{ID: "plan", Kind: "plan", Profile: "architect", Prompt: "plan.md"}},
			RequiredChecks: []string{"checks"}}, StageOrder: []string{"plan"}}
	canonical, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Digest = sha(canonical)
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	task := FrozenTask{Key: "plan"}
	inputSHA := sha([]byte(bundle.Digest + ":" + policySHA + ":plan"))
	got, err := decodeFrozenTask(task, bundle.Source.Commit, bundle.Digest, policySHA, inputSHA, bundleJSON, policyJSON)
	if err != nil || got.Stage.ID != "plan" || got.Profile.Harness != "codex" || got.Bundle.Digest != bundle.Digest {
		t.Fatalf("frozen task: %+v, %v", got, err)
	}
	bundle.Recipe.Profiles["architect"] = recipe.Profile{Harness: "claude-code", Model: "model", Effort: "high"}
	changed, _ := json.Marshal(bundle)
	if _, err := decodeFrozenTask(task, bundle.Source.Commit, bundle.Digest, policySHA, inputSHA, changed, policyJSON); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed profile accepted: %v", err)
	}
	if _, err := decodeFrozenTask(task, bundle.Source.Commit, bundle.Digest, policySHA, strings.Repeat("b", 64), bundleJSON, policyJSON); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed task input accepted: %v", err)
	}
}
