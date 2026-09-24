package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

type FrozenFile struct {
	Path   string
	SHA256 string
}

// RunTask is the durable stage state shown to a person inspecting a run.
type RunTask struct {
	ID              string
	Key             string
	State           string
	Generation      int64
	MaxAttempts     int32
	ActiveAttemptID *string
	DependsOn       []string
	Harness         string
	Model           string
	Effort          string
	Instructions    []FrozenFile
	Skills          []FrozenFile
}

// ListRunTasks returns only the selected organization's run graph. The caller
// must authorize the run before using this read model.
func (s *Store) ListRunTasks(ctx context.Context, orgID, runID string) ([]RunTask, error) {
	if !ids(orgID, runID) {
		return nil, ErrInvalid
	}
	var sourceCommit, bundleSHA, verificationSHA string
	var bundleJSON, verificationJSON []byte
	err := s.pool.QueryRow(ctx, `SELECT r.source_commit,r.bundle_sha256,r.verification_sha256,b.bundle_json,b.verification_json
		FROM workflow_runs r LEFT JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		WHERE r.organization_id=$1 AND r.id=$2`, orgID, runID).
		Scan(&sourceCommit, &bundleSHA, &verificationSHA, &bundleJSON, &verificationJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return []RunTask{}, nil
	}
	if err != nil {
		return nil, err
	}
	var bundle *recipe.Bundle
	if len(bundleJSON) != 0 || len(verificationJSON) != 0 {
		bundle, _, err = decodeFrozenBundle(sourceCommit, bundleSHA, verificationSHA, bundleJSON, verificationJSON)
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.pool.Query(ctx, `SELECT t.id,t.task_key,t.state,t.generation,t.max_attempts,t.active_attempt_id::text,
		t.input_sha256,
		ARRAY(SELECT parent.task_key FROM workflow_task_dependencies d
		JOIN workflow_tasks parent ON parent.organization_id=d.organization_id AND parent.id=d.depends_on_task_id
		WHERE d.organization_id=t.organization_id AND d.run_id=t.run_id AND d.task_id=t.id ORDER BY parent.task_key)
		FROM workflow_tasks t WHERE t.organization_id=$1 AND t.run_id=$2 ORDER BY t.created_at,t.id`, orgID, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []RunTask{}
	for rows.Next() {
		var task RunTask
		var inputSHA string
		if err := rows.Scan(&task.ID, &task.Key, &task.State, &task.Generation, &task.MaxAttempts,
			&task.ActiveAttemptID, &inputSHA, &task.DependsOn); err != nil {
			return nil, err
		}
		if bundle != nil {
			if inputSHA != sha([]byte(bundleSHA+":"+verificationSHA+":"+task.Key)) {
				return nil, ErrConflict
			}
			found := false
			for _, stage := range bundle.Recipe.Stages {
				if stage.ID != task.Key {
					continue
				}
				found = true
				if stage.Kind != "human_review" {
					profile, ok := bundle.Recipe.Profiles[stage.Profile]
					if !ok {
						return nil, ErrConflict
					}
					task.Harness, task.Model, task.Effort = profile.Harness, profile.Model, profile.Effort
					var err error
					task.Instructions, err = frozenFiles(profile.Instructions, bundle.Artifacts)
					if err != nil {
						return nil, err
					}
					task.Skills, err = frozenFiles(profile.Skills, bundle.Artifacts)
					if err != nil {
						return nil, err
					}
				}
				break
			}
			if !found {
				return nil, ErrConflict
			}
		}
		tasks = append(tasks, task)
	}
	return tasks, rows.Err()
}

func frozenFiles(paths []string, artifacts []recipe.Artifact) ([]FrozenFile, error) {
	files := make([]FrozenFile, 0, len(paths))
	for _, path := range paths {
		found := false
		for _, artifact := range artifacts {
			if artifact.Path == path {
				files = append(files, FrozenFile{Path: artifact.Path, SHA256: artifact.SHA256})
				found = true
				break
			}
		}
		if !found {
			return nil, ErrConflict
		}
	}
	return files, nil
}
