package workflow

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// ReadyTask is only a dispatch candidate. ReserveAttempt rechecks readiness
// under locks and is the sole authority for starting an attempt.
type ReadyTask struct {
	OrganizationID string
	ProjectID      string
	RunID          string
	TaskID         string
	Key            string
	Generation     int64
	MaxAttempts    int32
}

// ListReadyOrganizationIDs gives a dispatcher bounded, UUID-keyset tenant
// discovery. Reset afterID to empty after the last page to begin another pass.
func (s *Store) ListReadyOrganizationIDs(ctx context.Context, afterID string, limit int) ([]string, error) {
	if afterID == "" {
		afterID = "00000000-0000-0000-0000-000000000000"
	}
	if !ids(afterID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT r.organization_id FROM workflow_runs r
		JOIN workflow_tasks t ON t.organization_id=r.organization_id AND t.run_id=r.id
		JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		WHERE r.organization_id>$1 AND r.graph_sealed AND r.state IN ('queued','active')
		AND t.state='pending' AND t.active_attempt_id IS NULL AND t.generation<LEAST(20,t.max_attempts+t.extra_attempts)
		AND NOT EXISTS (SELECT 1 FROM workflow_task_dependencies d
			JOIN workflow_tasks parent ON parent.organization_id=d.organization_id
				AND parent.run_id=d.run_id AND parent.id=d.depends_on_task_id
			WHERE d.organization_id=t.organization_id AND d.run_id=t.run_id AND d.task_id=t.id
				AND parent.state<>'succeeded')
		ORDER BY r.organization_id LIMIT $2`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	organizations := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		organizations = append(organizations, id)
	}
	return organizations, rows.Err()
}

// ListReadyTasks is tenant-scoped and bounded. It never claims work.
func (s *Store) ListReadyTasks(ctx context.Context, orgID string, limit int) ([]ReadyTask, error) {
	if !ids(orgID) || limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT t.organization_id,r.project_id,t.run_id,t.id,t.task_key,t.generation,t.max_attempts
		FROM workflow_runs r
		JOIN workflow_tasks t ON t.organization_id=r.organization_id AND t.run_id=r.id
		JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		WHERE r.organization_id=$1 AND r.graph_sealed AND r.state IN ('queued','active')
		AND t.state='pending' AND t.active_attempt_id IS NULL AND t.generation<LEAST(20,t.max_attempts+t.extra_attempts)
		AND NOT EXISTS (SELECT 1 FROM workflow_task_dependencies d
			JOIN workflow_tasks parent ON parent.organization_id=d.organization_id
				AND parent.run_id=d.run_id AND parent.id=d.depends_on_task_id
			WHERE d.organization_id=t.organization_id AND d.run_id=t.run_id AND d.task_id=t.id
				AND parent.state<>'succeeded')
		-- A stage paced for gateway headroom waits until its retry time, so
		-- it cannot hold up other ready stages (docs/model-gateway-plan.md §5).
		AND NOT EXISTS (SELECT 1 FROM gateway_paced_tasks gp WHERE gp.organization_id=t.organization_id
			AND gp.task_id=t.id AND gp.retry_at>clock_timestamp())
		ORDER BY r.created_at,t.created_at,t.id LIMIT $2`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []ReadyTask{}
	for rows.Next() {
		var task ReadyTask
		if err := rows.Scan(&task.OrganizationID, &task.ProjectID, &task.RunID, &task.TaskID,
			&task.Key, &task.Generation, &task.MaxAttempts); err != nil {
			return nil, err
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

// FrozenTask selects only the task's signed-off recipe stage, its tool profile,
// and the immutable evidence bundle and human-controlled verification policy.
type FrozenTask struct {
	OrganizationID string
	ProjectID      string
	RunID          string
	TaskID         string
	Key            string
	RepositoryURL  string // Empty for legacy runs without a public source.
	SourceRef      string
	// GitConnectionID is the private source's frozen Git connection; empty
	// for public sources.
	GitConnectionID string
	Bundle          *recipe.Bundle
	Stage           recipe.Stage
	Profile         recipe.Profile
	Verification    VerificationPolicy
}

func (s *Store) LoadFrozenTask(ctx context.Context, orgID, runID, taskID string) (FrozenTask, error) {
	if !ids(orgID, runID, taskID) {
		return FrozenTask{}, ErrInvalid
	}
	var task FrozenTask
	var sealed bool
	var sourceCommit, bundleSHA, verificationSHA, inputSHA string
	var bundleJSON, verificationJSON []byte
	var repositoryURL, sourceRef *string
	err := s.pool.QueryRow(ctx, `SELECT r.project_id,r.graph_sealed,r.source_commit,r.bundle_sha256,r.verification_sha256,
		t.task_key,t.input_sha256,b.bundle_json,b.verification_json,b.repository_url,b.git_ref,COALESCE(b.git_connection_id,'')
		FROM workflow_tasks t JOIN workflow_runs r ON r.organization_id=t.organization_id AND r.id=t.run_id
		LEFT JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.id=$3`, orgID, runID, taskID).
		Scan(&task.ProjectID, &sealed, &sourceCommit, &bundleSHA, &verificationSHA,
			&task.Key, &inputSHA, &bundleJSON, &verificationJSON, &repositoryURL, &sourceRef, &task.GitConnectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return FrozenTask{}, ErrNotFound
	}
	if err != nil {
		return FrozenTask{}, err
	}
	if !sealed || len(bundleJSON) == 0 || len(verificationJSON) == 0 {
		return FrozenTask{}, ErrConflict
	}
	task.OrganizationID, task.RunID, task.TaskID = orgID, runID, taskID
	if repositoryURL != nil {
		task.RepositoryURL, task.SourceRef = *repositoryURL, *sourceRef
	}
	return decodeFrozenTask(task, sourceCommit, bundleSHA, verificationSHA, inputSHA, bundleJSON, verificationJSON)
}

func decodeFrozenTask(task FrozenTask, sourceCommit, bundleSHA, verificationSHA, inputSHA string, bundleJSON, verificationJSON []byte) (FrozenTask, error) {
	bundle, verification, err := decodeFrozenBundle(sourceCommit, bundleSHA, verificationSHA, bundleJSON, verificationJSON)
	if err != nil || inputSHA != sha([]byte(bundleSHA+":"+verificationSHA+":"+task.Key)) {
		return FrozenTask{}, ErrConflict
	}
	found := false
	for _, stage := range bundle.Recipe.Stages {
		if stage.ID != task.Key {
			continue
		}
		profile, ok := bundle.Recipe.Profiles[stage.Profile]
		if !ok || stage.Kind == "human_review" {
			return FrozenTask{}, ErrConflict
		}
		task.Stage, task.Profile, found = stage, profile, true
		break
	}
	if !found {
		return FrozenTask{}, ErrConflict
	}
	task.Bundle, task.Verification = bundle, verification
	return task, nil
}

func decodeFrozenBundle(sourceCommit, bundleSHA, verificationSHA string, bundleJSON, verificationJSON []byte) (*recipe.Bundle, VerificationPolicy, error) {
	var bundle recipe.Bundle
	var verification VerificationPolicy
	if json.Unmarshal(bundleJSON, &bundle) != nil || json.Unmarshal(verificationJSON, &verification) != nil ||
		bundle.SchemaVersion != "blaxsmith.bundle/v1alpha1" || bundle.Source.Commit != sourceCommit || bundle.Digest != bundleSHA {
		return nil, VerificationPolicy{}, ErrConflict
	}
	canonicalBundle := bundle
	canonicalBundle.Digest = ""
	encodedBundle, err := json.Marshal(canonicalBundle)
	if err != nil || sha(encodedBundle) != bundleSHA {
		return nil, VerificationPolicy{}, ErrConflict
	}
	encodedPolicy, err := validateVerification(verification, bundle.Recipe.RequiredChecks)
	if err != nil || sha(encodedPolicy) != verificationSHA {
		return nil, VerificationPolicy{}, ErrConflict
	}
	return &bundle, verification, nil
}
