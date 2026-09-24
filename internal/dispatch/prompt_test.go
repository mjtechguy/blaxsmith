package dispatch

import (
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestFrozenPrompt(t *testing.T) {
	bundle := &recipe.Bundle{Source: recipe.Source{Commit: strings.Repeat("a", 40), Scope: "src", Spec: "spec.md", Transcript: "transcript.md"},
		Artifacts: []recipe.Artifact{{Path: "spec.md", Data: []byte("requirements")},
			{Path: "transcript.md", Data: []byte("decisions")}, {Path: "src/AGENTS.md", Data: []byte("local rules")},
			{Path: "plan.md", Data: []byte("make a plan")}}}
	task := workflow.FrozenTask{Bundle: bundle, Stage: recipe.Stage{ID: "plan", Kind: "plan", Prompt: "plan.md"}}
	prompt, err := frozenPrompt(task)
	if err != nil || !strings.Contains(prompt, "requirements") || !strings.Contains(prompt, "local rules") ||
		!strings.Contains(prompt, "make a plan") || !strings.Contains(prompt, bundle.Source.Commit) {
		t.Fatalf("frozen prompt: %q, %v", prompt, err)
	}
	bundle.Artifacts = bundle.Artifacts[:3]
	if _, err := frozenPrompt(task); !errors.Is(err, tooladapter.ErrBlocked) {
		t.Fatalf("missing prompt artifact accepted: %v", err)
	}
}
