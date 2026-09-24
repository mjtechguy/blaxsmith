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
	Workflow  *workflow.Store
	Connector func(context.Context, workflow.Attempt) (*CommandExitConnector, error)
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
		batch.AfterOrganizationID, batch.AfterAttemptID = a.OrganizationID, a.ID
		batch.Examined++
		connector, err := s.Connector(ctx, a)
		if err == nil && connector == nil {
			err = workflow.ErrInvalid
		}
		if err == nil {
			err = connector.Reconcile(ctx, a)
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
