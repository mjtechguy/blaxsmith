package workflow

import (
	"context"
	"errors"
	"math"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrGoalStopped = errors.New("goal admission is paused or cancelled")
var ErrGoalControlDenied = errors.New("goal control requires its owner or an organization administrator")

type GoalControl struct {
	State                    string
	Version                  int64
	ActiveRuns, StoppingRuns int64
}

func (s *Store) GetGoalControl(ctx context.Context, org, goal string) (GoalControl, error) {
	var out GoalControl
	if !ids(org, goal) {
		return out, ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	err := s.pool.QueryRow(ctx, `SELECT control_state,control_version,
 (SELECT count(*) FROM workflow_goal_runs b JOIN workflow_runs r ON r.organization_id=b.organization_id AND r.id=b.run_id WHERE b.organization_id=g.organization_id AND b.goal_id=g.id AND r.state IN ('queued','active')),
 (SELECT count(*) FROM workflow_goal_runs b JOIN workflow_runs r ON r.organization_id=b.organization_id AND r.id=b.run_id WHERE b.organization_id=g.organization_id AND b.goal_id=g.id AND r.state='cancel_requested')
 FROM workflow_goals g WHERE organization_id=$1 AND id=$2`, org, goal).Scan(&out.State, &out.Version, &out.ActiveRuns, &out.StoppingRuns)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	// Cancellation intent persists. Confirmation is derived from reconciled runs,
	// never from a successful stop request or a disconnected coordinator.
	if out.State == "cancel_requested" && out.ActiveRuns == 0 && out.StoppingRuns == 0 {
		out.State = "cancelled"
	}
	return out, err
}

// ControlGoal serializes admission and control on the goal row. Pause drains
// existing attempts; cancel reuses run cancellation and its termination sweep.
// Neither changes frozen input, accepted work, or resource limits.
func (s *Store) ControlGoal(ctx context.Context, caller identity.Caller, goal, action, key string, expected int64) (int64, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, goal) || expected < 0 || expected == math.MaxInt64 || len(key) < 1 || len(key) > 128 || strings.ContainsRune(key, 0) || (action != "pause" && action != "resume" && action != "cancel") {
		return 0, ErrInvalid
	}
	if !CanLaunch(caller) {
		return 0, ErrGoalControlDenied
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err = lockCallerSession(ctx, tx, caller, true); err != nil {
		return 0, err
	}
	var owner, state string
	var version int64
	err = tx.QueryRow(ctx, `SELECT created_by,control_state,control_version FROM workflow_goals WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, goal).Scan(&owner, &state, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if owner != caller.PrincipalID && caller.Role != "owner" && caller.Role != "admin" {
		return 0, ErrGoalControlDenied
	}
	var oldAction string
	var oldExpected, oldVersion int64
	err = tx.QueryRow(ctx, `SELECT action,expected_version,version FROM workflow_goal_control_history WHERE organization_id=$1 AND goal_id=$2 AND principal_id=$3 AND request_key=$4`, caller.OrganizationID, goal, caller.PrincipalID, key).Scan(&oldAction, &oldExpected, &oldVersion)
	if err == nil {
		if oldAction != action || oldExpected != expected {
			return 0, ErrConflict
		}
		return oldVersion, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if version != expected || state == "cancel_requested" || (action == "pause" && state != "active") || (action == "resume" && state != "paused") {
		return 0, ErrConflict
	}
	next := map[string]string{"pause": "paused", "resume": "active", "cancel": "cancel_requested"}[action]
	if action == "cancel" {
		// Same lock order as admission: goal, then run. Read and lock the complete
		// active set before mutating; no run can attach while this goal is locked.
		rows, err := tx.Query(ctx, `SELECT r.id::text FROM workflow_goal_runs b JOIN workflow_runs r ON r.organization_id=b.organization_id AND r.id=b.run_id WHERE b.organization_id=$1 AND b.goal_id=$2 AND r.state IN ('queued','active') ORDER BY r.id FOR UPDATE OF r`, caller.OrganizationID, goal)
		if err != nil {
			return 0, err
		}
		runs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return 0, err
		}
		for _, run := range runs {
			if _, err = requestCancel(ctx, tx, caller.OrganizationID, run); err != nil {
				return 0, err
			}
		}
	}
	version++
	if _, err = tx.Exec(ctx, `UPDATE workflow_goals SET control_state=$3,control_version=$4 WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, goal, next, version); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO workflow_goal_control_history(organization_id,goal_id,version,expected_version,action,request_key,principal_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, caller.OrganizationID, goal, version, expected, action, key, caller.PrincipalID); err != nil {
		return 0, err
	}
	if err = audit(ctx, tx, caller, "goal."+action, goal); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}

// Lock the associated goal before a run when reopening completed work. A
// cancelled goal cannot acquire new active runs through review corrections.
func checkRunGoalControl(ctx context.Context, tx pgx.Tx, org, run string) error {
	var state string
	err := tx.QueryRow(ctx, `SELECT g.control_state FROM workflow_goals g JOIN workflow_goal_runs b ON b.organization_id=g.organization_id AND b.goal_id=g.id WHERE b.organization_id=$1 AND b.run_id=$2 FOR UPDATE OF g`, org, run).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state != "active" {
		return ErrGoalStopped
	}
	return nil
}
