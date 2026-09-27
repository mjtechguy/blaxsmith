// Package anvil supplies the default factory's planning behavior. It does not
// own task execution, authority, or project verification policy.
package anvil

import "github.com/mjtechguy/blaxsmith/internal/interact"

const Version = "0.1.0"

// StarterQuestions are a guided intake, not model-generated analysis. All are
// optional; an MVP can start from its brief. Answers record planning preferences
// and never silently change project checks or authorize work.
func StarterQuestions() []interact.Interaction {
	return []interact.Interaction{
		{ID: "codebase", Kind: "question", Title: "What are we building on?", BodyMD: "Describe the existing behavior to preserve, or the starting point for a new project.", AllowFreeText: true,
			Options: []interact.Option{{ID: "existing", Label: "Existing codebase", Description: "Understand current architecture, conventions, tests, and behavior before changing code."}, {ID: "new", Label: "New project", Description: "Establish the stack, boundaries, and a small working foundation."}}},
		{ID: "depth", Kind: "question", Title: "How thorough should this work be?", BodyMD: "This guides planning depth. You choose the actual required, advisory, or disabled checks before launching.", AllowFreeText: true,
			Options: []interact.Option{{ID: "focused", Label: "Focused MVP", Description: "Small scope, direct implementation, and the checks you select."}, {ID: "thorough", Label: "Thorough delivery", Description: "Detailed tasks, code review, failure cases, and end-to-end evidence where applicable."}}},
		{ID: "success", Kind: "question", Title: "What would a successful result look like?", BodyMD: "Give concrete examples: what should a user be able to do, what must stay unchanged, and how should we verify it?", AllowFreeText: true},
		{ID: "constraints", Kind: "question", Title: "Which rules and limits should shape the plan?", BodyMD: "Include stack choices, AGENTS.md or other rule files, model preferences, time or spending limits, and anything out of scope.", AllowFreeText: true},
	}
}
