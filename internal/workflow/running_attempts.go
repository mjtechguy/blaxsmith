package workflow

import (
	"context"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

const zeroUUID = "00000000-0000-0000-0000-000000000000"

// ListRunningAttempts scans only current, sealed AX owners. A caller resumes
// after the last returned (organization ID, attempt ID) pair and resets both
// cursors after an empty page. It does not claim or mutate an attempt.
func (s *Store) ListRunningAttempts(ctx context.Context, afterOrgID, afterAttemptID string, limit int) ([]Attempt, error) {
	ctx = tenant.System(ctx)
	if afterOrgID == "" {
		afterOrgID = zeroUUID
	}
	if afterAttemptID == "" {
		afterAttemptID = zeroUUID
	}
	if !ids(afterOrgID, afterAttemptID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.organization_id,a.run_id,a.task_id,a.generation,a.fence_token,a.state
		FROM workflow_attempts a
		JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id
		JOIN workflow_tasks t ON t.organization_id=a.organization_id AND t.id=a.task_id
		WHERE (a.organization_id,a.id)>($1::uuid,$2::uuid)
		AND a.state='running' AND t.state='running' AND t.active_attempt_id=a.id
		AND r.state IN ('active','cancel_requested') AND r.graph_sealed
		ORDER BY a.organization_id,a.id LIMIT $3`, afterOrgID, afterAttemptID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.ID, &a.OrganizationID, &a.RunID, &a.TaskID,
			&a.OwnerGeneration, &a.FenceToken, &a.State); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}

// ListUnresolvedAttempts finds current owners whose AX launch may have been
// interrupted by a connector restart. It is bounded and does not claim work.
func (s *Store) ListUnresolvedAttempts(ctx context.Context, afterOrgID, afterAttemptID string, limit int) ([]Attempt, error) {
	ctx = tenant.System(ctx)
	if afterOrgID == "" {
		afterOrgID = zeroUUID
	}
	if afterAttemptID == "" {
		afterAttemptID = zeroUUID
	}
	if !ids(afterOrgID, afterAttemptID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.organization_id,a.run_id,a.task_id,a.generation,a.fence_token,a.state
		FROM workflow_attempts a
		JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id
		JOIN workflow_tasks t ON t.organization_id=a.organization_id AND t.id=a.task_id
		WHERE (a.organization_id,a.id)>($1::uuid,$2::uuid)
		AND a.state IN ('reserved','starting','reconciling') AND t.state=a.state AND t.active_attempt_id=a.id
		AND r.state IN ('active','cancel_requested') AND r.graph_sealed
		ORDER BY a.organization_id,a.id LIMIT $3`, afterOrgID, afterAttemptID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	attempts := []Attempt{}
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&a.ID, &a.OrganizationID, &a.RunID, &a.TaskID,
			&a.OwnerGeneration, &a.FenceToken, &a.State); err != nil {
			return nil, err
		}
		attempts = append(attempts, a)
	}
	return attempts, rows.Err()
}
