package interact

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// Allowance edits are human-only. Goal writers cannot increase their own
// delegated resource envelope through machine credentials.
func (s *Store) SetGoalAllowance(ctx context.Context, caller identity.Caller, goal string, expected int64, runs, attempts int, deadline *time.Time, repeatedFailures, noProgressSeconds int, resetStallWindow bool) error {
	if !ids(goal) || expected < 0 || runs < 0 || runs > 10000 || attempts < 0 || attempts > 100000 || repeatedFailures < 0 || repeatedFailures > 100 || noProgressSeconds < 0 || noProgressSeconds > 2592000 || (noProgressSeconds > 0 && noProgressSeconds < 60) {
		return workflow.ErrInvalid
	}
	if deadline != nil && (deadline.Year() < 2000 || deadline.Year() > 2100) {
		return workflow.ErrInvalid
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockRunControl(ctx, tx, caller); err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT credential_kind FROM identity_sessions WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, caller.SessionID).Scan(&kind); err != nil {
		return err
	}
	if kind != "browser" {
		return ErrDenied
	}
	var owner string
	err = tx.QueryRow(ctx, `SELECT created_by FROM workflow_goals WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, goal).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return workflow.ErrNotFound
	}
	if err != nil {
		return err
	}
	if owner != caller.PrincipalID && caller.Role != "owner" && caller.Role != "admin" {
		return ErrDenied
	}
	var version int64
	err = tx.QueryRow(ctx, `SELECT version FROM workflow_goal_allowances WHERE organization_id=$1 AND goal_id=$2`, caller.OrganizationID, goal).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if version != expected {
		return workflow.ErrConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_goal_allowances(organization_id,goal_id,version,max_runs,max_attempts,admit_until,principal_id,max_repeated_check_failures,no_progress_seconds,stall_reset_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,CASE WHEN $10 THEN clock_timestamp() END)
 ON CONFLICT(organization_id,goal_id) DO UPDATE SET version=EXCLUDED.version,max_runs=EXCLUDED.max_runs,max_attempts=EXCLUDED.max_attempts,admit_until=EXCLUDED.admit_until,principal_id=EXCLUDED.principal_id,updated_at=clock_timestamp(),max_repeated_check_failures=EXCLUDED.max_repeated_check_failures,no_progress_seconds=EXCLUDED.no_progress_seconds,stall_reset_at=CASE WHEN $10 THEN EXCLUDED.stall_reset_at ELSE workflow_goal_allowances.stall_reset_at END`, caller.OrganizationID, goal, version+1, runs, attempts, deadline, caller.PrincipalID, repeatedFailures, noProgressSeconds, resetStallWindow)
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workflow_goal_allowance_history SELECT * FROM workflow_goal_allowances WHERE organization_id=$1 AND goal_id=$2`, caller.OrganizationID, goal); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,'goal.allowance.changed',$3)`, caller.OrganizationID, caller.PrincipalID, goal); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
