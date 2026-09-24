package workflow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
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
	// Browser admission rechecks these against the live session and the
	// project settings after Git preparation, in the run creation transaction.
	Caller              *identity.Caller
	SourceRepositoryURL string
	SourceRef           string
	VerificationVersion int64
	// RecipeVersionID names the library version whose bytes are
	// Source.RecipeData; admission rechecks it with the caller's access.
	RecipeVersionID string
}

// CreateFrozenRun compiles committed inputs, then atomically persists their
// exact bytes, the dependency graph, and a dispatch seal. It does not grant
// access, start workers, or approve results; its caller must authorize first.
func (s *Store) CreateFrozenRun(ctx context.Context, in FrozenRunInput) (Run, error) {
	if !ids(in.OrganizationID, in.ProjectID) || len(in.LaunchKey) < 1 || len(in.LaunchKey) > 128 ||
		(in.RecipeVersionID != "" && (!ids(in.RecipeVersionID) || in.Caller == nil || in.Source.RecipeData == nil)) {
		return Run{}, ErrInvalid
	}
	bundle, err := recipe.Freeze(ctx, in.Source)
	if err != nil {
		return Run{}, fmt.Errorf("%w: %v", ErrRecipe, err)
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
	var gitConnectionID string // frozen private-source connection; empty = public
	if in.Caller != nil {
		caller := *in.Caller
		if caller.OrganizationID != in.OrganizationID || !ids(caller.PrincipalID, caller.SessionID) ||
			(caller.Role != "owner" && caller.Role != "admin" && caller.Role != "member") ||
			in.SourceRepositoryURL == "" || in.VerificationVersion < 1 {
			return Run{}, ErrInvalid
		}
		var role string
		err = tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
			JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
			JOIN identity_principals p ON p.id=s.principal_id
			JOIN identity_organizations o ON o.id=s.organization_id
			WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
			AND m.role=$4 AND m.role IN ('owner','admin','member') AND $5::timestamptz>clock_timestamp()
			AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
			AND m.state='active' AND p.state='active'
			AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
			AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
			FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
			caller.Role, caller.AccessExpires).Scan(&role)
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, ErrFenced
		}
		if err != nil {
			return Run{}, err
		}
		var repositoryURL, sourceRef string
		err = tx.QueryRow(ctx, `SELECT repository_url,git_ref,COALESCE(git_connection_id,'') FROM workflow_project_sources
			WHERE organization_id=$1 AND project_id=$2 FOR SHARE`, in.OrganizationID, in.ProjectID).
			Scan(&repositoryURL, &sourceRef, &gitConnectionID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (repositoryURL != in.SourceRepositoryURL || sourceRef != in.SourceRef)) {
			return Run{}, ErrConflict
		}
		if err != nil {
			return Run{}, err
		}
		var version int64
		var currentPolicy []byte
		err = tx.QueryRow(ctx, `SELECT version,policy_json FROM workflow_project_verification
			WHERE organization_id=$1 AND project_id=$2 FOR SHARE`, in.OrganizationID, in.ProjectID).
			Scan(&version, &currentPolicy)
		if errors.Is(err, pgx.ErrNoRows) {
			return Run{}, ErrConflict
		}
		if err != nil {
			return Run{}, err
		}
		var parsed VerificationPolicy
		if err := json.Unmarshal(currentPolicy, &parsed); err != nil {
			return Run{}, err
		}
		canonical, err := json.Marshal(parsed)
		if err != nil || version != in.VerificationVersion || !bytes.Equal(canonical, policyJSON) {
			return Run{}, ErrConflict
		}
		if in.RecipeVersionID != "" {
			library, err := s.launchableVersion(ctx, tx, caller, in.ProjectID, in.RecipeVersionID)
			if err != nil {
				return Run{}, err
			}
			if library.SHA256 != sha(in.Source.RecipeData) || library.FrozenPath != in.Source.Recipe {
				return Run{}, ErrConflict
			}
		}
	}
	var runID, initiator string
	if in.Caller != nil {
		initiator = in.Caller.PrincipalID
	}
	err = tx.QueryRow(ctx, `INSERT INTO workflow_runs
		(organization_id,project_id,launch_key,source_commit,bundle_sha256,verification_sha256,initiator_principal_id,recipe_version_id)
		VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,'')::uuid)
		ON CONFLICT (organization_id,project_id,launch_key) DO NOTHING RETURNING id`,
		in.OrganizationID, in.ProjectID, in.LaunchKey, bundle.Source.Commit, bundle.Digest, policySHA, initiator, in.RecipeVersionID).Scan(&runID)
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
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref,git_connection_id)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),CASE WHEN $5='' THEN NULL ELSE $6 END,NULLIF($7,''))`,
		in.OrganizationID, run.ID, bundleJSON, policyJSON, in.SourceRepositoryURL, in.SourceRef, gitConnectionID); err != nil {
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
	if in.Caller != nil {
		if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
			(organization_id,actor_kind,actor_id,action,subject_id)
			VALUES ($1,'principal',$2,'workflow.run.launched',$3)`,
			in.OrganizationID, in.Caller.PrincipalID, run.ID); err != nil {
			return Run{}, err
		}
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
