package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
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
	Kind            string
	LoopWith        string // Set only on a stage that owns a loop.
	MaxCycles       int32  // Loop cap including human-granted raises.
	LoopCycles      int32  // Corrections the loop has requested.
}

// ListRunTasks returns only the selected organization's run graph. The caller
// must authorize the run before using this read model.
func (s *Store) ListRunTasks(ctx context.Context, orgID, runID string) ([]RunTask, error) {
	ctx = tenant.Org(ctx, orgID)
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
	type loopCount struct{ used, granted int32 }
	loops := map[string]loopCount{}
	rows, err := s.pool.Query(ctx, `SELECT stage,sum(used)::integer,sum(granted)::integer FROM (
		SELECT source_stage AS stage,count(DISTINCT COALESCE(decision_id,id)) AS used,0 AS granted FROM workflow_corrections
			WHERE organization_id=$1 AND run_id=$2 GROUP BY source_stage
		UNION ALL SELECT stage_key,0,granted_cycles FROM workflow_escalations
			WHERE organization_id=$1 AND run_id=$2 AND resolution='raise_cap') counts GROUP BY stage`, orgID, runID)
	if err != nil {
		return nil, err
	}
	var loopStage string
	var loop loopCount
	if _, err := pgx.ForEachRow(rows, []any{&loopStage, &loop.used, &loop.granted}, func() error {
		loops[loopStage] = loop
		return nil
	}); err != nil {
		return nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT t.id,t.task_key,t.state,t.generation,t.max_attempts,t.active_attempt_id::text,
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
				task.Kind = stage.Kind
				if stage.Loop != nil {
					count := loops[stage.ID]
					task.LoopWith, task.MaxCycles, task.LoopCycles = stage.Loop.With, int32(stage.Loop.MaxCycles)+count.granted, count.used
				}
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
