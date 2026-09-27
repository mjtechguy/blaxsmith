package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestFrozenPrompt(t *testing.T) {
	artifact := func(name, body string) recipe.Artifact {
		sum := sha256.Sum256([]byte(body))
		return recipe.Artifact{Path: name, Data: []byte(body), SHA256: hex.EncodeToString(sum[:])}
	}
	bundle := &recipe.Bundle{Recipe: recipe.Recipe{Documents: []string{"spec.md", "transcript.md"}}, Source: recipe.Source{Commit: strings.Repeat("a", 40), Scope: "src"},
		Artifacts: []recipe.Artifact{artifact("spec.md", "requirements"), artifact("transcript.md", "decisions"),
			artifact("src/AGENTS.md", "local rules"), artifact("instructions.md", "design intent"),
			artifact("skills/evidence/SKILL.md", "evidence workflow"), artifact("plan.md", "make a plan")}}
	task := workflow.FrozenTask{Bundle: bundle, Stage: recipe.Stage{ID: "plan", Kind: "plan", Prompt: "plan.md"},
		Profile: recipe.Profile{Instructions: []string{"instructions.md"}, Skills: []string{"skills/evidence/SKILL.md"}}}
	prompt, manifest, err := frozenPrompt(task, "", task.Bundle.Source.Commit)
	if err != nil || !strings.Contains(prompt, "requirements") || !strings.Contains(prompt, "local rules") ||
		!strings.Contains(prompt, "make a plan") || !strings.Contains(prompt, "design intent") ||
		strings.Contains(prompt, "evidence workflow") || !strings.Contains(prompt, "Selected Skills are installed") ||
		!strings.Contains(prompt, bundle.Source.Commit) || len(manifest) != 6 {
		t.Fatalf("frozen prompt: %q, %v", prompt, err)
	}
	handed, _, err := frozenPrompt(task, "\n### Upstream stage plan\nFinal summary:\nplanned\n", task.Bundle.Source.Commit)
	if err != nil || !strings.HasPrefix(handed, prompt) || !strings.Contains(handed, "untrusted context") || !strings.Contains(handed, "planned") {
		t.Fatalf("handoff prompt: %q, %v", handed, err)
	}
	if _, _, err := frozenPrompt(task, "bad\x00", task.Bundle.Source.Commit); !errors.Is(err, tooladapter.ErrBlocked) {
		t.Fatalf("NUL handoff accepted: %v", err)
	}
	bundle.Artifacts[4].Data = []byte("tampered")
	if _, _, err := frozenPrompt(task, "", task.Bundle.Source.Commit); !errors.Is(err, tooladapter.ErrBlocked) {
		t.Fatalf("tampered skill artifact accepted: %v", err)
	}
	bundle.Artifacts = append(bundle.Artifacts[:4], bundle.Artifacts[5:]...)
	if _, _, err := frozenPrompt(task, "", task.Bundle.Source.Commit); !errors.Is(err, tooladapter.ErrBlocked) {
		t.Fatalf("missing skill artifact accepted: %v", err)
	}
}

func TestPlatformPromptDoesNotRequireGitFile(t *testing.T) {
	data := []byte("saved context")
	sum := sha256.Sum256(data)
	name := ".blaxsmith/platform/plan.md"
	task := workflow.FrozenTask{Stage: recipe.Stage{ID: "plan", Kind: "plan", Prompt: name}, Bundle: &recipe.Bundle{Artifacts: []recipe.Artifact{{Path: name, Data: data, SHA256: hex.EncodeToString(sum[:]), Source: "goal:one@1"}}}}
	prompt, manifest, err := frozenPrompt(task, "", task.Bundle.Source.Commit)
	if err != nil || len(manifest) != 0 || !strings.Contains(prompt, "saved context") || !strings.Contains(prompt, "goal:one@1") {
		t.Fatalf("platform bytes: %s %v %v", prompt, manifest, err)
	}
	task.Bundle.Artifacts[0].Data = []byte("tampered")
	if _, _, err = frozenPrompt(task, "", task.Bundle.Source.Commit); !errors.Is(err, tooladapter.ErrBlocked) {
		t.Fatalf("tampered context accepted: %v", err)
	}
}
