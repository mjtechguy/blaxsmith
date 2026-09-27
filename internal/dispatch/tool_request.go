package dispatch

import (
	"errors"
	"fmt"

	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

var ErrStageInputs = errors.New("stage worker inputs are not ready")

// PrepareToolRequest is shared by launch admission, dispatch and reconstruction.
// It performs no I/O, reserves no attempt and never releases credentials.
func PrepareToolRequest(frozen workflow.FrozenTask, approved workflow.ApprovedToolRuntime, handoff, input string) (request tooladapter.Request, err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: stage %q: %w", ErrStageInputs, frozen.Stage.ID, err)
		}
	}()
	prompt, artifacts, err := frozenPrompt(frozen, handoff, input)
	if err != nil {
		return request, fmt.Errorf("%w: frozen instructions are missing, changed, or exceed the prompt limit", err)
	}
	if err := tooladapter.ValidateSkillFiles(frozen.Profile.Skills, frozen.Bundle.Artifacts); err != nil {
		return request, err
	}
	mount, _, err := extensionMount(frozen.Bundle, frozen.Stage)
	if err != nil {
		return request, err
	}
	request = tooladapter.Request{
		// Account for the real UUID and checkout directory in the encoded limit.
		AttemptID: "00000000-0000-0000-0000-000000000000", SourceDirectory: "source",
		RepositoryURL: frozen.RepositoryURL, SourceRef: frozen.SourceRef, SourceCommit: input,
		Runtime: approved.Runtime, Profile: frozen.Profile, Prompt: prompt, FrozenArtifacts: artifacts,
		TimeoutSeconds:    min(frozen.Bundle.Recipe.Limits.TimeoutSeconds, approved.MaxTimeoutSeconds),
		MaxRuntimeSeconds: frozen.Bundle.Recipe.Limits.MaxRuntimeSeconds, MaxOutputBytes: approved.MaxOutputBytes, Extension: mount,
	}
	// Connection selection is a platform concern; the worker receives only its lease.
	request.Profile.Connection = ""
	_, err = tooladapter.Command(request)
	return request, err
}
