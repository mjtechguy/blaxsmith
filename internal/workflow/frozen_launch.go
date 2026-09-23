package workflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// VerificationPolicy is resolved from trusted organization/project settings,
// never from the recipe repository. The check runner is still a separate gate.
type VerificationPolicy struct {
	SchemaVersion string              `json:"schema_version"`
	Checks        []VerificationCheck `json:"checks"`
}

type VerificationCheck struct {
	ID      string   `json:"id"`
	Command []string `json:"command"`
}

type FrozenRunInput struct {
	OrganizationID string
	ProjectID      string
	LaunchKey      string
	Source         recipe.Input // Repo must be selected by the authorized server, not the browser.
	Verification   VerificationPolicy
}

// CreateFrozenRun compiles committed inputs, then atomically persists their
// exact bytes, the dependency graph, and a dispatch seal. It does not grant
// access, start workers, or approve results; its caller must authorize first.
func (s *Store) CreateFrozenRun(ctx context.Context, in FrozenRunInput) (Run, error) {
	if !ids(in.OrganizationID, in.ProjectID) || len(in.LaunchKey) < 1 || len(in.LaunchKey) > 128 {
		return Run{}, ErrInvalid
	}
	bundle, err := recipe.Freeze(ctx, in.Source)
	if err != nil {
		return Run{}, fmt.Errorf("freeze recipe: %w", err)
	}
	policyJSON, err := validateVerification(in.Verification, bundle.Recipe.RequiredChecks)
	if err != nil {
		return Run{}, err
	}
	bundleJSON, err := json.Marshal(bundle)
	if err != nil {
		return Run{}, err
	}
	policySHA := sha(policyJSON)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(ctx)
	var runID string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_runs
		(organization_id,project_id,launch_key,source_commit,bundle_sha256,verification_sha256)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (organization_id,project_id,launch_key) DO NOTHING RETURNING id`,
		in.OrganizationID, in.ProjectID, in.LaunchKey, bundle.Source.Commit, bundle.Digest, policySHA).Scan(&runID)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("create frozen run: %w", err)
	}
	run, err := getRun(ctx, tx, in.OrganizationID, in.ProjectID, in.LaunchKey)
	if err != nil {
		return Run{}, err
	}
	if run.SourceCommit != bundle.Source.Commit || run.BundleSHA256 != bundle.Digest || run.VerificationSHA256 != policySHA {
		return Run{}, ErrConflict
	}
	if !created {
		var sealed bool
		if err := tx.QueryRow(ctx, `SELECT graph_sealed FROM workflow_runs
			WHERE organization_id=$1 AND id=$2`, in.OrganizationID, run.ID).Scan(&sealed); err != nil {
			return Run{}, err
		}
		if !sealed {
			return Run{}, ErrConflict
		}
		return run, tx.Commit(ctx)
	}
	if err := event(ctx, tx, in.OrganizationID, run.ID, "", "", "run.created"); err != nil {
		return Run{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_run_bundles
		(organization_id,run_id,bundle_json,verification_json) VALUES ($1,$2,$3,$4)`,
		in.OrganizationID, run.ID, bundleJSON, policyJSON); err != nil {
		return Run{}, err
	}
	stageByID := make(map[string]recipe.Stage, len(bundle.Recipe.Stages))
	for _, stage := range bundle.Recipe.Stages {
		stageByID[stage.ID] = stage
	}
	taskByStage := make(map[string]string, len(bundle.StageOrder))
	for _, stageID := range bundle.StageOrder {
		stage := stageByID[stageID]
		if stage.Kind == "human_review" {
			continue // Human approval follows execution and evidence collection.
		}
		inputSHA := sha([]byte(bundle.Digest + ":" + policySHA + ":" + stageID))
		var taskID string
		if err := tx.QueryRow(ctx, `INSERT INTO workflow_tasks
			(organization_id,run_id,task_key,input_sha256,max_attempts)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`, in.OrganizationID, run.ID,
			stageID, inputSHA, bundle.Recipe.Limits.MaxCorrectionCycles+1).Scan(&taskID); err != nil {
			return Run{}, fmt.Errorf("create task %s: %w", stageID, err)
		}
		taskByStage[stageID] = taskID
		if err := event(ctx, tx, in.OrganizationID, run.ID, taskID, "", "task.created"); err != nil {
			return Run{}, err
		}
		for _, dependency := range stage.DependsOn {
			parent := taskByStage[dependency]
			if parent == "" {
				return Run{}, fmt.Errorf("task %s has unresolved dependency %s", stageID, dependency)
			}
			if _, err := tx.Exec(ctx, `INSERT INTO workflow_task_dependencies
				(organization_id,run_id,task_id,depends_on_task_id) VALUES ($1,$2,$3,$4)`,
				in.OrganizationID, run.ID, taskID, parent); err != nil {
				return Run{}, err
			}
		}
	}
	if len(taskByStage) == 0 {
		return Run{}, ErrInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true
		WHERE organization_id=$1 AND id=$2`, in.OrganizationID, run.ID); err != nil {
		return Run{}, err
	}
	if err := event(ctx, tx, in.OrganizationID, run.ID, "", "", "run.graph_sealed"); err != nil {
		return Run{}, err
	}
	return run, tx.Commit(ctx)
}

func validateVerification(policy VerificationPolicy, required []string) ([]byte, error) {
	if policy.SchemaVersion != "blaxsmith.verification/v1alpha1" || len(policy.Checks) == 0 || len(policy.Checks) > 64 {
		return nil, ErrInvalid
	}
	seen := make(map[string]bool, len(policy.Checks))
	for _, check := range policy.Checks {
		if !keyPattern.MatchString(check.ID) || seen[check.ID] || len(check.Command) == 0 || len(check.Command) > 32 {
			return nil, ErrInvalid
		}
		seen[check.ID] = true
		for _, part := range check.Command {
			if part == "" || len(part) > 4096 || strings.ContainsRune(part, 0) {
				return nil, ErrInvalid
			}
		}
	}
	for _, id := range required {
		if !seen[id] {
			return nil, ErrInvalid
		}
	}
	data, err := json.Marshal(policy)
	if err != nil || len(data) > 1<<20 {
		return nil, ErrInvalid
	}
	return data, nil
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
