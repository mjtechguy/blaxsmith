package workflow

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

type GoalCheckpoint struct {
	ID, GoalID, RunID, PackageID, CandidateRevision, Title, PrincipalID string
	Sequence, PlanVersion, GoalRevision                                 int64
	CreatedAt                                                           time.Time
	CurrentAcceptance                                                   bool // False when a later package/decision supersedes acceptance.
}

const checkpointColumns = `c.id,c.goal_id,c.run_id,c.package_id,c.candidate_revision,c.title,c.principal_id,c.sequence,c.plan_version,c.goal_revision,c.created_at,
 (r.state='succeeded' AND r.review_package_id=c.package_id AND (p.acceptance_mode='policy' OR d.action='approve')) IS TRUE`
const checkpointJoins = ` FROM workflow_goal_checkpoints c JOIN workflow_runs r ON r.organization_id=c.organization_id AND r.id=c.run_id
 JOIN workflow_review_packages p ON p.organization_id=c.organization_id AND p.id=c.package_id
 LEFT JOIN workflow_review_decisions d ON d.organization_id=p.organization_id AND d.package_id=p.id`

func scanCheckpoint(row pgx.Row) (GoalCheckpoint, error) {
	var c GoalCheckpoint
	err := row.Scan(&c.ID, &c.GoalID, &c.RunID, &c.PackageID, &c.CandidateRevision, &c.Title, &c.PrincipalID, &c.Sequence, &c.PlanVersion, &c.GoalRevision, &c.CreatedAt, &c.CurrentAcceptance)
	return c, err
}

// RecordGoalCheckpoint records an already accepted, code-producing run. It
// cannot approve work or reinterpret task packets as individual task evidence.
func (s *Store) RecordGoalCheckpoint(ctx context.Context, caller identity.Caller, goal, run, packageID, title, key string, expected int64) (GoalCheckpoint, error) {
	title = strings.TrimSpace(title)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, goal, run, packageID) || expected < 1 || len(key) < 1 || len(key) > 128 || strings.ContainsRune(key, 0) || !utf8.ValidString(title) || utf8.RuneCountInString(title) < 1 || utf8.RuneCountInString(title) > 300 || strings.ContainsRune(title, 0) {
		return GoalCheckpoint{}, ErrInvalid
	}
	if !CanLaunch(caller) {
		return GoalCheckpoint{}, ErrGoalControlDenied
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return GoalCheckpoint{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockCallerSession(ctx, tx, caller, true); err != nil {
		return GoalCheckpoint{}, err
	}
	var owner string
	var revision int64
	if err = tx.QueryRow(ctx, `SELECT created_by,revision FROM workflow_goals WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, goal).Scan(&owner, &revision); errors.Is(err, pgx.ErrNoRows) {
		return GoalCheckpoint{}, ErrNotFound
	} else if err != nil {
		return GoalCheckpoint{}, err
	}
	if owner != caller.PrincipalID && caller.Role != "owner" && caller.Role != "admin" {
		return GoalCheckpoint{}, ErrGoalControlDenied
	}
	old, err := scanCheckpoint(tx.QueryRow(ctx, `SELECT `+checkpointColumns+checkpointJoins+` WHERE c.organization_id=$1 AND c.goal_id=$2 AND c.principal_id=$3 AND c.request_key=$4`, caller.OrganizationID, goal, caller.PrincipalID, key))
	if err == nil {
		var oldExpected int64
		if err = tx.QueryRow(ctx, `SELECT expected_goal_revision FROM workflow_goal_checkpoints WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, old.ID).Scan(&oldExpected); err != nil {
			return GoalCheckpoint{}, err
		}
		if old.RunID != run || old.PackageID != packageID || old.Title != title || oldExpected != expected {
			return GoalCheckpoint{}, ErrConflict
		}
		return old, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return GoalCheckpoint{}, err
	}
	if revision != expected {
		return GoalCheckpoint{}, ErrConflict
	}
	var state string
	var planVersion, goalRevision int64
	err = tx.QueryRow(ctx, `SELECT r.state,COALESCE(g.plan_version,0),g.goal_revision FROM workflow_goal_runs g JOIN workflow_runs r ON r.organization_id=g.organization_id AND r.id=g.run_id WHERE g.organization_id=$1 AND g.goal_id=$2 AND g.run_id=$3 FOR UPDATE OF r`, caller.OrganizationID, goal, run).Scan(&state, &planVersion, &goalRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return GoalCheckpoint{}, ErrNotFound
	}
	if err != nil {
		return GoalCheckpoint{}, err
	}
	if state != "succeeded" || planVersion == 0 {
		return GoalCheckpoint{}, ErrConflict
	}
	review, err := readCurrentReview(ctx, tx, caller.OrganizationID, run)
	if err != nil {
		return GoalCheckpoint{}, err
	}
	if review.ID != packageID || (review.AcceptanceMode != "policy" && (review.Decision == nil || review.Decision.Action != "approve")) {
		return GoalCheckpoint{}, ErrConflict
	}
	bundle, err := runBundle(ctx, tx, caller.OrganizationID, run)
	if err != nil {
		return GoalCheckpoint{}, err
	}
	code := false
	for _, stage := range bundle.Recipe.Stages {
		code = code || stage.Kind == "implement"
	}
	if !code {
		return GoalCheckpoint{}, ErrConflict
	}
	var seq int64
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(sequence),0)+1 FROM workflow_goal_checkpoints WHERE organization_id=$1 AND goal_id=$2`, caller.OrganizationID, goal).Scan(&seq); err != nil {
		return GoalCheckpoint{}, err
	}
	if seq > 10000 {
		return GoalCheckpoint{}, ErrInvalid
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_goal_checkpoints(organization_id,goal_id,sequence,run_id,package_id,plan_version,goal_revision,expected_goal_revision,candidate_revision,title,request_key,principal_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12) ON CONFLICT(organization_id,goal_id,package_id) DO NOTHING RETURNING id`, caller.OrganizationID, goal, seq, run, packageID, planVersion, goalRevision, expected, review.IntegratedCommit, title, key, caller.PrincipalID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return GoalCheckpoint{}, ErrConflict
	}
	if err != nil {
		return GoalCheckpoint{}, err
	}
	if err = audit(ctx, tx, caller, "goal.checkpoint", id); err != nil {
		return GoalCheckpoint{}, err
	}
	result, err := scanCheckpoint(tx.QueryRow(ctx, `SELECT `+checkpointColumns+checkpointJoins+` WHERE c.organization_id=$1 AND c.id=$2`, caller.OrganizationID, id))
	if err != nil {
		return GoalCheckpoint{}, err
	}
	return result, tx.Commit(ctx)
}

func (s *Store) ListGoalCheckpoints(ctx context.Context, org, goal string, before int64) ([]GoalCheckpoint, bool, error) {
	if !ids(org, goal) || before < 0 {
		return nil, false, ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_goals WHERE organization_id=$1 AND id=$2)`, org, goal).Scan(&exists); err != nil {
		return nil, false, err
	}
	if !exists {
		return nil, false, ErrNotFound
	}
	rows, err := s.pool.Query(ctx, `SELECT `+checkpointColumns+checkpointJoins+` WHERE c.organization_id=$1 AND c.goal_id=$2 AND ($3::bigint=0 OR c.sequence<$3) ORDER BY c.sequence DESC LIMIT 21`, org, goal, before)
	if err != nil {
		return nil, false, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GoalCheckpoint, error) { return scanCheckpoint(row) })
	if len(out) > 20 {
		return out[:20], true, err
	}
	return out, false, err
}

// GoalCheckpointSource is a preview read. Admission repeats it while locking the
// accepted run, serializing against package supersession and review decisions.
func (s *Store) GoalCheckpointSource(ctx context.Context, org, goal, checkpoint, repository string) (string, error) {
	return checkpointSource(tenant.Org(ctx, org), s.pool, org, goal, checkpoint, repository, false)
}

func checkpointSource(ctx context.Context, q reviewQuerier, org, goal, checkpoint, repository string, lock bool) (string, error) {
	if !ids(org, goal, checkpoint) || repository == "" {
		return "", ErrInvalid
	}
	query := `SELECT c.candidate_revision,b.repository_url,
 (r.state='succeeded' AND r.review_package_id=c.package_id AND (p.acceptance_mode='policy' OR d.action='approve')) IS TRUE`
	query += checkpointJoins + ` JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
 WHERE c.organization_id=$1 AND c.goal_id=$2 AND c.id=$3`
	if lock {
		query += ` FOR SHARE OF r`
	}
	var commit string
	var recordedRepository *string
	var accepted bool
	err := q.QueryRow(ctx, query, org, goal, checkpoint).Scan(&commit, &recordedRepository, &accepted)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if !accepted || recordedRepository == nil || *recordedRepository != repository {
		return "", ErrConflict
	}
	return commit, nil
}
