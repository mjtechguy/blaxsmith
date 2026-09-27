package axbridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// CompletionSweep makes one bounded pass over current running owners. The
// caller builds each connector from that attempt's frozen task and runtime
// binding, then passes the returned cursor into the next pass. An empty cursor
// starts a new pass. One blocked attempt cannot starve later attempts.
type CompletionSweep struct {
	// Verify handles deterministic verify stages before any guest exit/result
	// read. A handled successful call is followed by normal actor-stop proof.
	Verify    func(context.Context, workflow.Attempt, *Bridge) (bool, error)
	Workflow  *workflow.Store
	Connector func(context.Context, workflow.Attempt) (*CommandExitConnector, error)
	// Result reads the guest's bounded result file after a signed clean exit;
	// nil data means the guest wrote none. A nil Result keeps clean exits
	// pending for an external verifier. The result is untrusted handoff text:
	// it advances the graph only after actor stop is proved.
	Result func(context.Context, workflow.Attempt) ([]byte, error)
	// Deliver runs before the result is recorded: it pushes a code-producing
	// stage's commit to the run branch and sets result.Revision to the
	// accepted revision. workflow.ErrInvalid discards the result.
	Deliver func(context.Context, workflow.Attempt, *workflow.AttemptResult) error
}

type CompletionBatch struct {
	AfterOrganizationID string
	AfterAttemptID      string
	Examined            int
	Stopped             int
	Waiting             int
	VerificationPending int
	Stale               int
}

func (s *CompletionSweep) Sweep(ctx context.Context, afterOrgID, afterAttemptID string, limit int) (CompletionBatch, error) {
	if s == nil || s.Workflow == nil || s.Connector == nil {
		return CompletionBatch{}, workflow.ErrInvalid
	}
	attempts, err := s.Workflow.ListRunningAttempts(ctx, afterOrgID, afterAttemptID, limit)
	if err != nil {
		return CompletionBatch{}, err
	}
	var batch CompletionBatch
	var failures []error
	for _, a := range attempts {
		if err := ctx.Err(); err != nil {
			return batch, errors.Join(append(failures, err)...)
		}
		batch.AfterOrganizationID, batch.AfterAttemptID = a.OrganizationID, a.ID
		batch.Examined++
		connector, err := s.Connector(ctx, a)
		if err == nil && (connector == nil || connector.Bridge == nil) {
			err = workflow.ErrInvalid
		}
		handled := false
		if err == nil {
			var run, state string
			var sealed bool
			run, state, sealed, err = s.Workflow.CurrentAttempt(ctx, a)
			if err == nil && (state != "running" || !sealed) {
				err = workflow.ErrFenced
			}
			if err == nil && run == "cancel_requested" {
				handled = true // cancellation must not enter the verifier's claim gate
			} else if err == nil && s.Verify != nil {
				handled, err = s.Verify(ctx, a, connector.Bridge)
				if err != nil && ctx.Err() == nil {
					// Cancellation during verification interrupts its child context.
					// Stop using the still-live sweep context and normal actor proof.
					if current, _, _, readErr := s.Workflow.CurrentAttempt(ctx, a); readErr == nil && current == "cancel_requested" {
						handled, err = true, nil
					}
				}
			}
		}
		if err == nil {
			if handled {
				err = connector.Bridge.StopKnown(ctx, a)
			} else {
				err = connector.Reconcile(ctx, a)
			}
		}
		if errors.Is(err, ErrVerificationPending) && s.Result != nil {
			err = s.accept(ctx, connector, a)
		}
		switch {
		case err == nil:
			batch.Stopped++
		case errors.Is(err, ErrPending):
			batch.Waiting++
		case errors.Is(err, ErrVerificationPending):
			batch.VerificationPending++
		case errors.Is(err, workflow.ErrFenced):
			batch.Stale++
		default:
			failures = append(failures, fmt.Errorf("complete attempt %s: %w", a.ID, err))
		}
	}
	return batch, errors.Join(failures...)
}

// SweepCancelled never runs checks or reads guest results. It has its own
// cursor and deadline so an unrelated verification cannot delay cancellation.
func (s *CompletionSweep) SweepCancelled(ctx context.Context, afterOrgID, afterAttemptID string, limit int) (CompletionBatch, error) {
	if s == nil || s.Workflow == nil || s.Connector == nil {
		return CompletionBatch{}, workflow.ErrInvalid
	}
	attempts, err := s.Workflow.ListCancellingAttempts(ctx, afterOrgID, afterAttemptID, limit)
	if err != nil {
		return CompletionBatch{}, err
	}
	var batch CompletionBatch
	var failures []error
	for _, a := range attempts {
		if err := ctx.Err(); err != nil {
			return batch, errors.Join(append(failures, err)...)
		}
		batch.AfterOrganizationID, batch.AfterAttemptID = a.OrganizationID, a.ID
		batch.Examined++
		connector, err := s.Connector(ctx, a)
		if err == nil && (connector == nil || connector.Bridge == nil) {
			err = workflow.ErrInvalid
		}
		if err == nil {
			err = connector.Bridge.StopKnown(ctx, a)
		}
		switch {
		case err == nil:
			batch.Stopped++
		case errors.Is(err, ErrPending):
			batch.Waiting++
		case errors.Is(err, workflow.ErrFenced):
			batch.Stale++
		default:
			failures = append(failures, fmt.Errorf("cancel attempt %s: %w", a.ID, err))
		}
	}
	return batch, errors.Join(failures...)
}

// accept records the guest result once, then proves the actor gone. A
// malformed result is discarded and the stop takes the normal retry path.
func (s *CompletionSweep) accept(ctx context.Context, connector *CommandExitConnector, a workflow.Attempt) error {
	recorded, err := s.Workflow.HasAttemptResult(ctx, a)
	if err != nil {
		return err
	}
	if !recorded {
		data, err := s.Result(ctx, a)
		if err != nil {
			return err
		}
		result := workflow.AttemptResult{}
		if data != nil {
			result, err = workflow.ParseAttemptResult(data)
		}
		if err == nil && s.Deliver != nil {
			err = s.Deliver(ctx, a, &result)
		}
		if err == nil {
			err = s.Workflow.RecordAttemptResult(ctx, a, result)
		}
		if err != nil && !errors.Is(err, workflow.ErrInvalid) {
			return err
		}
	}
	return connector.Bridge.StopKnown(ctx, a)
}
