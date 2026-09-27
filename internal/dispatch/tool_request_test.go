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

func TestPrepareToolRequest(t *testing.T) {
	approved := workflow.ApprovedToolRuntime{Runtime: tooladapter.Runtime{
		Harness: "codex", Image: "runner@sha256:" + strings.Repeat("a", 64),
		Binary: "/opt/blaxsmith/bin/codex", BinarySHA256: strings.Repeat("b", 64), Version: "0.156.1",
		Supported: []tooladapter.ModelEffort{{Model: "gpt-6-luna", Effort: "xhigh"}},
	}, MaxTimeoutSeconds: 60, MaxOutputBytes: 1024}
	const skill = "---\nname: review\ndescription: Review existing code\n---\nFollow the selected rules."
	for _, tc := range []struct {
		name, prompt, skill, extraSkill, want string
		tamper                                bool
	}{
		{name: "valid", prompt: "Build the feature", skill: skill},
		{name: "large instructions", prompt: strings.Repeat("x", 130<<10), skill: skill, want: "encoded worker request"},
		{name: "JSON expansion", prompt: strings.Repeat("<", 30<<10), skill: skill, want: "encoded worker request"},
		{name: "missing metadata", prompt: "Build", skill: "Review this", want: "missing skill frontmatter"},
		{name: "wrong name", prompt: "Build", skill: strings.Replace(skill, "name: review", "name: other", 1), want: "name must match"},
		{name: "tool grants", prompt: "Build", skill: strings.Replace(skill, "description:", "allowed-tools: Bash\ndescription:", 1), want: "harness-specific skill frontmatter"},
		{name: "duplicate names", prompt: "Build", skill: skill, extraSkill: "other/review/SKILL.md", want: "names must be unique"},
		{name: "orphan support", prompt: "Build", skill: skill, extraSkill: "other/help.md", want: "under a selected SKILL.md"},
		{name: "changed frozen bytes", prompt: "Build", skill: skill, tamper: true, want: "frozen instructions"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := func(name, body, source string) recipe.Artifact {
				sum := sha256.Sum256([]byte(body))
				return recipe.Artifact{Path: name, Data: []byte(body), SHA256: hex.EncodeToString(sum[:]), Source: source}
			}
			bundle := &recipe.Bundle{Source: recipe.Source{Commit: strings.Repeat("a", 40), Scope: "."},
				Recipe: recipe.Recipe{Limits: recipe.Limits{TimeoutSeconds: 120}, Documents: []string{".blaxsmith/platform/context.md"}},
				Artifacts: []recipe.Artifact{artifact("task.md", tc.prompt, ""), artifact("rules.md", "Keep existing behavior", ""),
					artifact(".blaxsmith/platform/context.md", "Saved goal context", "goal:one@1"),
					artifact("skills/review/SKILL.md", tc.skill, ""), artifact("skills/review/help.md", "Supporting guidance", "")}}
			frozen := workflow.FrozenTask{Bundle: bundle, RepositoryURL: "https://github.com/owner/repo", SourceRef: "main",
				Stage: recipe.Stage{ID: "implement", Kind: "implement", Prompt: "task.md"},
				Profile: recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh", Instructions: []string{"rules.md"},
					Skills: []string{"skills/review/SKILL.md", "skills/review/help.md"}}}
			if tc.extraSkill != "" {
				frozen.Profile.Skills = append(frozen.Profile.Skills, tc.extraSkill)
				bundle.Artifacts = append(bundle.Artifacts, artifact(tc.extraSkill, skill, ""))
			}
			if tc.tamper {
				bundle.Artifacts[3].Data = []byte("changed")
			}
			request, err := PrepareToolRequest(frozen, approved, "Previous stage summary", strings.Repeat("c", 40))
			if tc.want != "" {
				if !errors.Is(err, ErrStageInputs) || !errors.Is(err, tooladapter.ErrBlocked) ||
					!strings.Contains(err.Error(), `stage "implement"`) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("wanted stage-specific %q blocker, got %v", tc.want, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if request.TimeoutSeconds != 60 || len(request.AttemptID) != 36 || request.SourceDirectory != "source" ||
				request.SourceCommit != strings.Repeat("c", 40) || request.SourceRef != "main" || len(request.FrozenArtifacts) != 4 {
				t.Fatalf("incorrect frozen request: %+v", request)
			}
			for _, text := range []string{tc.prompt, "Keep existing behavior", "Saved goal context", "Previous stage summary", bundle.Source.Commit} {
				if !strings.Contains(request.Prompt, text) {
					t.Fatalf("missing frozen context %q", text)
				}
			}
			if strings.Contains(request.Prompt, skill) {
				t.Fatal("skill content must use native discovery")
			}
		})
	}
}
