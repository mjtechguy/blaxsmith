package interact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// GoalSnapshot is an exact planning input, including current decisions and all
// saved context messages. Never silently truncate an assignment.
type GoalSnapshot struct {
	Goal     Goal        `json:"goal"`
	Messages []GoalEntry `json:"messages"`
}

func (s *Store) SnapshotGoal(ctx context.Context, org, id string, revision int64) (GoalSnapshot, []byte, error) {
	var snapshot GoalSnapshot
	before := int64(0)
	for {
		g, entries, more, err := s.GetGoal(ctx, org, id, before)
		if err != nil {
			return snapshot, nil, err
		}
		if g.Revision != revision {
			return snapshot, nil, workflow.ErrConflict
		}
		snapshot.Goal = g
		notes := []GoalEntry{}
		for _, e := range entries {
			if e.Kind == "message" {
				notes = append(notes, e)
			}
		}
		snapshot.Messages = append(notes, snapshot.Messages...)
		data, _ := json.Marshal(snapshot)
		if len(data) > 256<<10 {
			return snapshot, nil, fmt.Errorf("%w: goal context exceeds 256 KiB; split this goal", workflow.ErrInvalid)
		}
		if !more {
			return snapshot, data, nil
		}
		before = entries[0].Sequence
	}
}
func (s GoalSnapshot) References() map[string]bool {
	refs := map[string]bool{"brief": true}
	for _, q := range s.Goal.Questions {
		if q.State == "answered" {
			refs["question:"+q.ID] = true
		}
	}
	for _, e := range s.Messages {
		refs[fmt.Sprintf("message:%d", e.Sequence)] = true
	}
	return refs
}

type GoalRun struct {
	ID, State, CheckpointID   string
	GoalRevision, PlanVersion int64
}

func (s *Store) GoalRuns(ctx context.Context, org, goal string) ([]GoalRun, error) {
	ctx = tenant.Org(ctx, org)
	if !ids(org, goal) {
		return nil, workflow.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT r.id,r.state,g.goal_revision,COALESCE(g.plan_version,0),COALESCE(g.checkpoint_id::text,'') FROM workflow_goal_runs g JOIN workflow_runs r ON r.organization_id=g.organization_id AND r.id=g.run_id WHERE g.organization_id=$1 AND g.goal_id=$2 ORDER BY g.created_at DESC LIMIT 20`, org, goal)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (GoalRun, error) {
		var r GoalRun
		err := row.Scan(&r.ID, &r.State, &r.GoalRevision, &r.PlanVersion, &r.CheckpointID)
		return r, err
	})
}

type GoalPlan struct {
	Version, GoalRevision                      int64
	JSON, SHA256, RunID, EvidenceID, CreatedAt string
}

func (s *Store) GoalPlans(ctx context.Context, org, goal string, before int64) ([]GoalPlan, bool, error) {
	ctx = tenant.Org(ctx, org)
	if !ids(org, goal) || before < 0 {
		return nil, false, workflow.ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT version,goal_revision,content,sha256,COALESCE(run_id::text,''),COALESCE(evidence_id::text,''),created_at FROM workflow_goal_plans WHERE organization_id=$1 AND goal_id=$2 AND ($3::bigint=0 OR version<$3) ORDER BY version DESC LIMIT 21`, org, goal, before)
	if err != nil {
		return nil, false, err
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (GoalPlan, error) {
		var p GoalPlan
		var content []byte
		var created time.Time
		err := row.Scan(&p.Version, &p.GoalRevision, &content, &p.SHA256, &p.RunID, &p.EvidenceID, &created)
		p.JSON = string(content)
		p.CreatedAt = created.UTC().Format(time.RFC3339Nano)
		return p, err
	})
	more := len(plans) > 20
	if more {
		plans = plans[:20]
	}
	return plans, more, err
}

// PlanEvidence reads only the completed, current planning attempt of a run
// attached to this goal. A worker's draft is not accepted implementation proof.
func (s *Store) PlanEvidence(ctx context.Context, org, goal, evidenceID, artifactKey string) ([]byte, string, int64, error) {
	ctx = tenant.Org(ctx, org)
	if !ids(org, goal, evidenceID) || !goalText(artifactKey, 1, 64) {
		return nil, "", 0, workflow.ErrInvalid
	}
	var content []byte
	var run string
	var revision int64
	err := s.pool.QueryRow(ctx, `SELECT e.content,g.run_id,g.goal_revision FROM workflow_goal_runs g
 JOIN workflow_evidence e ON e.organization_id=g.organization_id AND e.run_id=g.run_id
 JOIN workflow_tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id
 JOIN workflow_attempts a ON a.organization_id=e.organization_id AND a.id=e.attempt_id
 WHERE g.organization_id=$1 AND g.goal_id=$2 AND e.id=$3 AND e.kind='artifact' AND e.origin_key=$4
 AND EXISTS(SELECT 1 FROM workflow_run_bundles b,jsonb_array_elements(b.bundle_json->'recipe'->'stages') stage WHERE b.organization_id=g.organization_id AND b.run_id=g.run_id AND stage->>'id'=t.task_key AND stage->>'kind'='plan') AND t.state='succeeded' AND a.state='succeeded' AND a.generation=t.generation`, org, goal, evidenceID, artifactKey).Scan(&content, &run, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		err = workflow.ErrNotFound
	}
	return content, run, revision, err
}

type GoalPlanInput struct {
	GoalID, RequestKey, RunID, EvidenceID, ArtifactKey string
	GoalRevision, ExpectedVersion                      int64
	Content                                            []byte
}

func (s *Store) SaveGoalPlan(ctx context.Context, caller identity.Caller, in GoalPlanInput) (int64, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(in.GoalID) || !goalText(in.RequestKey, 1, 128) || in.GoalRevision < 1 || in.ExpectedVersion < 0 || len(in.Content) < 1 || len(in.Content) > 256<<10 || !json.Valid(in.Content) || (in.RunID != "" && (!ids(in.RunID, in.EvidenceID) || !goalText(in.ArtifactKey, 1, 64))) || ((in.RunID == "") != (in.EvidenceID == "")) {
		return 0, workflow.ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err = lockRunControl(ctx, tx, caller); err != nil {
		return 0, err
	}
	var revision int64
	err = tx.QueryRow(ctx, `SELECT revision FROM workflow_goals WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, in.GoalID).Scan(&revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, workflow.ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(in.Content)
	digest := hex.EncodeToString(sum[:])
	var version, oldRevision int64
	var oldSHA, oldRun, oldEvidence string
	err = tx.QueryRow(ctx, `SELECT version,goal_revision,sha256,COALESCE(run_id::text,''),COALESCE(evidence_id::text,'') FROM workflow_goal_plans WHERE organization_id=$1 AND goal_id=$2 AND principal_id=$3 AND request_key=$4`, caller.OrganizationID, in.GoalID, caller.PrincipalID, in.RequestKey).Scan(&version, &oldRevision, &oldSHA, &oldRun, &oldEvidence)
	if err == nil {
		if oldSHA != digest || oldRevision != in.GoalRevision || oldRun != in.RunID || oldEvidence != in.EvidenceID {
			return 0, workflow.ErrConflict
		}
		return version, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if revision != in.GoalRevision {
		return 0, workflow.ErrConflict
	}
	if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM workflow_goal_plans WHERE organization_id=$1 AND goal_id=$2`, caller.OrganizationID, in.GoalID).Scan(&version); err != nil {
		return 0, err
	}
	if version != in.ExpectedVersion {
		return 0, workflow.ErrConflict
	}
	if in.RunID != "" {
		var valid bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_goal_runs g JOIN workflow_evidence e ON e.organization_id=g.organization_id AND e.run_id=g.run_id JOIN workflow_tasks t ON t.organization_id=e.organization_id AND t.id=e.task_id JOIN workflow_attempts a ON a.organization_id=e.organization_id AND a.id=e.attempt_id WHERE g.organization_id=$1 AND g.goal_id=$2 AND g.run_id=$3 AND g.goal_revision=$4 AND e.id=$5 AND e.kind='artifact' AND e.origin_key=$6 AND t.state='succeeded' AND EXISTS(SELECT 1 FROM workflow_run_bundles b,jsonb_array_elements(b.bundle_json->'recipe'->'stages') stage WHERE b.organization_id=g.organization_id AND b.run_id=g.run_id AND stage->>'id'=t.task_key AND stage->>'kind'='plan') AND a.state='succeeded' AND a.generation=t.generation)`, caller.OrganizationID, in.GoalID, in.RunID, in.GoalRevision, in.EvidenceID, in.ArtifactKey).Scan(&valid)
		if err != nil {
			return 0, err
		}
		if !valid {
			return 0, workflow.ErrConflict
		}
	}
	version++
	_, err = tx.Exec(ctx, `INSERT INTO workflow_goal_plans(organization_id,goal_id,version,goal_revision,request_key,content,sha256,run_id,evidence_id,principal_id) VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,'')::uuid,NULLIF($9,'')::uuid,$10)`, caller.OrganizationID, in.GoalID, version, in.GoalRevision, in.RequestKey, in.Content, digest, in.RunID, in.EvidenceID, caller.PrincipalID)
	if err != nil {
		return 0, err
	}
	if err = goalAudit(ctx, tx, caller, in.GoalID, "plan.saved"); err != nil {
		return 0, err
	}
	return version, tx.Commit(ctx)
}
