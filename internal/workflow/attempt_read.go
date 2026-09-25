package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// CurrentAttempt checks the persisted owner before any external dispatch or
// recovery operation. It does not authorize the caller or prove AX state.
func (s *Store) CurrentAttempt(ctx context.Context, a Attempt) (runState, attemptState string, graphSealed bool, err error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) {
		return "", "", false, ErrInvalid
	}
	var taskState, token string
	var activeID *string
	var generation int64
	err = s.pool.QueryRow(ctx, `SELECT r.state,r.graph_sealed,t.state,t.active_attempt_id,a.state,a.fence_token,a.generation
		FROM workflow_runs r JOIN workflow_tasks t ON (t.organization_id=r.organization_id AND t.run_id=r.id)
		JOIN workflow_attempts a ON (a.organization_id=t.organization_id AND a.task_id=t.id)
		WHERE r.organization_id=$1 AND r.id=$2 AND t.id=$3 AND a.id=$4`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&runState, &graphSealed, &taskState, &activeID, &attemptState, &token, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", false, ErrFenced
	}
	if err != nil {
		return "", "", false, err
	}
	if activeID == nil || *activeID != a.ID || token != a.FenceToken || generation != a.OwnerGeneration || taskState != attemptState {
		return "", "", false, ErrFenced
	}
	return runState, attemptState, graphSealed, nil
}

// ConfirmRecovered returns a reconciled AX actor to initialization. It remains
// unresolved until WaitWorkspaceReady confirms WorkspaceReady=SetupComplete.
func (s *Store) ConfirmRecovered(ctx context.Context, a Attempt) error {
	ctx = tenant.Org(ctx, a.OrganizationID)
	return s.transition(ctx, a, "reconciling", "starting", "attempt.starting")
}
