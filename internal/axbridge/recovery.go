package axbridge

import (
	"context"
	"errors"
	"fmt"

	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// RecoverySweep reconciles bounded pages of AX writes that may have been
// interrupted by a connector restart. It only reads AX; missing resources stay
// fenced for explicit resolution.
type RecoverySweep struct {
	Workflow *workflow.Store
	Bridge   func(context.Context, workflow.Attempt) (*Bridge, error)
}

type RecoveryBatch struct {
	AfterOrganizationID string
	AfterAttemptID      string
	Examined            int
	Recovered           int
	Waiting             int
	Stale               int
}

func (s *RecoverySweep) Sweep(ctx context.Context, afterOrgID, afterAttemptID string, limit int) (RecoveryBatch, error) {
	if s == nil || s.Workflow == nil || s.Bridge == nil {
		return RecoveryBatch{}, workflow.ErrInvalid
	}
	attempts, err := s.Workflow.ListUnresolvedAttempts(ctx, afterOrgID, afterAttemptID, limit)
	if err != nil {
		return RecoveryBatch{}, err
	}
	var batch RecoveryBatch
	var failures []error
	for _, attempt := range attempts {
		batch.AfterOrganizationID, batch.AfterAttemptID = attempt.OrganizationID, attempt.ID
		batch.Examined++
		if err := ctx.Err(); err != nil {
			return batch, errors.Join(append(failures, err)...)
		}
		if attempt.State == "reserved" {
			if err := s.Workflow.MarkUnknown(ctx, attempt); err != nil {
				if errors.Is(err, workflow.ErrFenced) {
					batch.Stale++
					continue
				}
				failures = append(failures, fmt.Errorf("fence interrupted attempt %s: %w", attempt.ID, err))
				continue
			}
			attempt.State = "reconciling"
		}
		bridge, err := s.Bridge(ctx, attempt)
		if err == nil && bridge == nil {
			err = workflow.ErrInvalid
		}
		if err == nil && bridge.Workflow != s.Workflow {
			err = workflow.ErrInvalid
		}
		if err == nil {
			_, err = bridge.ReconcileUnknown(ctx, attempt)
		}
		switch {
		case err == nil:
			batch.Recovered++
		case errors.Is(err, ErrPending):
			batch.Waiting++
		case errors.Is(err, workflow.ErrFenced):
			batch.Stale++
		default:
			failures = append(failures, fmt.Errorf("recover attempt %s: %w", attempt.ID, err))
		}
	}
	return batch, errors.Join(failures...)
}
