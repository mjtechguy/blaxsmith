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
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// VerificationPolicy is resolved from trusted organization/project settings,
// never from the recipe repository. The check runner is still a separate gate.
type VerificationPolicy struct {
	SchemaVersion string              `json:"schema_version"`
	Checks        []VerificationCheck `json:"checks"`
	Preset        string              `json:"preset,omitempty"`
}

type VerificationCheck struct {
	ID           string   `json:"id"`
	Category     string   `json:"category,omitempty"`
	Command      []string `json:"command"`
	TrustedPaths []string `json:"trusted_paths,omitempty"`
	Mode         string   `json:"mode,omitempty"` // Empty means required.
}

func (c VerificationCheck) Required() bool { return c.Mode == "" || c.Mode == "required" }

func (p VerificationPolicy) ActiveChecks() []VerificationCheck {
	checks := make([]VerificationCheck, 0, len(p.Checks))
	for _, c := range p.Checks {
		if c.Mode != "off" {
			checks = append(checks, c)
		}
	}
	return checks
}

// PreviewSource compiles the same inputs as admission and checks extension grants.
// It persists nothing and confers no authority to launch later.
func (s *Store) PreviewSource(ctx context.Context, caller identity.Caller, projectID string, source recipe.Input) (*recipe.Bundle, error) {
	if !CanLaunch(caller) {
		return nil, ErrProjectDenied
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if _, err := s.GetProject(ctx, caller.OrganizationID, projectID); err != nil {
		return nil, err
	}
	resolver := s.extensionResolver(caller.OrganizationID)
	source.ResolveExtension = resolver.Resolve
	bundle, err := recipe.Freeze(ctx, source)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRecipe, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := resolver.recordRunExtensions(ctx, tx, &caller, projectID, "", bundle.Extensions); err != nil {
		return nil, err
	}
	return bundle, nil
}

var ErrPreviewChanged = errors.New("launch inputs changed since preview; refresh the preview")

// CheckPreview allows an unpreviewed launch, or requires both exact digests.
func CheckPreview(expectedBundle, expectedPolicy, bundle, policy string) error {
	if expectedBundle == "" && expectedPolicy == "" {
		return nil
	}
	for _, digest := range []string{expectedBundle, expectedPolicy} {
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size || digest != strings.ToLower(digest) {
			return ErrInvalid
		}
	}
	if expectedBundle != bundle || expectedPolicy != policy {
		return ErrPreviewChanged
	}
	return nil
}

// PreviewVerification validates the effective checks against the frozen graph.
func PreviewVerification(bundle *recipe.Bundle, policy VerificationPolicy) (string, error) {
	data, err := launchVerification(bundle, policy)
	if err != nil {
		return "", err
	}
	return sha(data), nil
}

func launchVerification(bundle *recipe.Bundle, policy VerificationPolicy) ([]byte, error) {
	policyJSON, err := validateVerification(policy, platformRequiredChecks(bundle))
	if err != nil {
		return nil, err
	}
	if len(policy.ActiveChecks()) > 0 {
		ancestors := map[string]map[string]bool{}
		implement := ""
		for _, stage := range bundle.Recipe.Stages {
			if stage.Kind == "implement" {
				implement = stage.ID
			}
		}
		verified := false
		for _, key := range bundle.StageOrder {
			stage, _ := findStage(bundle, key)
			before := map[string]bool{}
			for _, dep := range stage.DependsOn {
				before[dep] = true
				for id := range ancestors[dep] {
					before[id] = true
				}
			}
			ancestors[key] = before
			if stage.Kind == "verify" && (implement == "" || before[implement]) {
				verified = true
			}
		}
		if !verified {
			return nil, fmt.Errorf("%w: selected project checks require a verify stage after implementation", ErrRecipe)
		}
	}
	return policyJSON, nil
}

type FrozenRunInput struct {
	CheckpointID               string
	GoalID                     string
	GoalRevision               int64
	GoalPlanVersion            int64
	GoalPlanSHA256             string
	ExpectedBundleSHA256       string
	ExpectedVerificationSHA256 string
	OrganizationID             string
	ProjectID                  string
	LaunchKey                  string
	Source                     recipe.Input // Repo must be selected by the authorized server, not the browser.
	Verification               VerificationPolicy
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
	ctx = tenant.Org(ctx, in.OrganizationID)
	if !ids(in.OrganizationID, in.ProjectID) || len(in.LaunchKey) < 1 || len(in.LaunchKey) > 128 ||
		(in.CheckpointID != "" && (in.GoalID == "" || !ids(in.CheckpointID))) ||
		(in.GoalPlanVersion < 0 || (in.GoalPlanVersion > 0 && (in.GoalID == "" || !hashPattern.MatchString(in.GoalPlanSHA256)))) ||
		(in.GoalID != "" && (!ids(in.GoalID) || in.GoalRevision < 1 || in.Caller == nil)) ||
		(in.RecipeVersionID != "" && (!ids(in.RecipeVersionID) || in.Caller == nil || in.Source.RecipeData == nil)) {
		return Run{}, ErrInvalid
	}
	// Extension templates resolve only against this organization's installed
	// versions; the caller's grant is checked in the creation transaction.
	extensions := s.extensionResolver(in.OrganizationID)
	source := in.Source
	source.CheckpointID = in.CheckpointID
	source.ResolveExtension = extensions.Resolve
	bundle, err := recipe.Freeze(ctx, source)
	if err != nil {
		return Run{}, fmt.Errorf("%w: %v", ErrRecipe, err)
	}
	policyJSON, err := launchVerification(bundle, in.Verification)
	if err != nil {
		return Run{}, err
	}
	if err := CheckPreview(in.ExpectedBundleSHA256, in.ExpectedVerificationSHA256, bundle.Digest, sha(policyJSON)); err != nil {
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
			AND (s.credential_kind='service' OR o.mfa_policy<>'required' OR s.mfa_level='totp')
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
	if in.GoalID != "" {
		var revision int64
		if err := tx.QueryRow(ctx, `SELECT revision FROM workflow_goals WHERE organization_id=$1 AND project_id=$2 AND id=$3 FOR UPDATE`, in.OrganizationID, in.ProjectID, in.GoalID).Scan(&revision); errors.Is(err, pgx.ErrNoRows) {
			return Run{}, ErrNotFound
		} else if err != nil {
			return Run{}, err
		}
		if revision != in.GoalRevision {
			return Run{}, ErrConflict
		}
		if in.GoalPlanVersion > 0 {
			var matches bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_goal_plans WHERE organization_id=$1 AND goal_id=$2 AND version=$3 AND goal_revision=$4 AND sha256=$5)`, in.OrganizationID, in.GoalID, in.GoalPlanVersion, in.GoalRevision, in.GoalPlanSHA256).Scan(&matches); err != nil {
				return Run{}, err
			}
			if !matches {
				return Run{}, ErrConflict
			}
		}
	}
	checkoutRef := in.SourceRef
	if in.CheckpointID != "" {
		commit, err := checkpointSource(ctx, tx, in.OrganizationID, in.GoalID, in.CheckpointID, in.SourceRepositoryURL, true)
		if err != nil {
			return Run{}, err
		}
		if commit != bundle.Source.Commit {
			return Run{}, ErrConflict
		}
		checkoutRef = commit
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
	if created && in.GoalID != "" {
		if err := checkGoalAllowance(ctx, tx, in.OrganizationID, in.GoalID, false); err != nil {
			return Run{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO workflow_goal_runs(organization_id,goal_id,run_id,goal_revision,plan_version,checkpoint_id) VALUES($1,$2,$3,$4,NULLIF($5,0),NULLIF($6,'')::uuid)`, in.OrganizationID, in.GoalID, run.ID, in.GoalRevision, in.GoalPlanVersion, in.CheckpointID); err != nil {
			return Run{}, err
		}
	}
	if !created {
		if in.GoalID != "" {
			var associated bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_goal_runs WHERE organization_id=$1 AND run_id=$2 AND goal_id=$3 AND goal_revision=$4 AND COALESCE(plan_version,0)=$5 AND COALESCE(checkpoint_id::text,'')=$6)`, in.OrganizationID, run.ID, in.GoalID, in.GoalRevision, in.GoalPlanVersion, in.CheckpointID).Scan(&associated); err != nil {
				return Run{}, err
			}
			if !associated {
				return Run{}, ErrConflict
			}
		}
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
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref,git_connection_id,configured_git_ref)
		VALUES ($1,$2,$3,$4,NULLIF($5,''),CASE WHEN $5='' THEN NULL ELSE $6 END,NULLIF($7,''),CASE WHEN $5='' THEN NULL ELSE $8 END)`,
		in.OrganizationID, run.ID, bundleJSON, policyJSON, in.SourceRepositoryURL, checkoutRef, gitConnectionID, in.SourceRef); err != nil {
		return Run{}, err
	}
	if err := extensions.recordRunExtensions(ctx, tx, in.Caller, in.ProjectID, run.ID, bundle.Extensions); err != nil {
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
	if policy.SchemaVersion != "blaxsmith.verification/v1alpha1" || len(policy.Checks) > 64 {
		return nil, ErrInvalid
	}
	if policy.Preset != "" && policy.Preset != "custom" && policy.Preset != "mvp" && policy.Preset != "balanced" && policy.Preset != "thorough" {
		return nil, ErrInvalid
	}
	seen := make(map[string]bool, len(policy.Checks))
	requiredChecks := map[string]bool{}
	for _, check := range policy.Checks {
		switch check.Category {
		case "", "build", "test", "review", "e2e", "security", "performance", "other":
		default:
			return nil, ErrInvalid
		}
		if policy.Preset != "" && policy.Preset != "custom" {
			mode := "required"
			if policy.Preset == "mvp" {
				if check.Category == "test" || check.Category == "other" || check.Category == "" {
					mode = "advisory"
				} else if check.Category != "build" {
					mode = "off"
				}
			}
			if policy.Preset == "balanced" && (check.Category == "review" || check.Category == "e2e" || check.Category == "security" || check.Category == "performance") {
				mode = "advisory"
			}
			actual := check.Mode
			if actual == "" {
				actual = "required"
			}
			if actual != mode {
				return nil, ErrInvalid
			}
		}
		if check.Mode != "" && check.Mode != "required" && check.Mode != "advisory" && check.Mode != "off" {
			return nil, ErrInvalid
		}
		if !keyPattern.MatchString(check.ID) || seen[check.ID] || len(check.Command) == 0 || len(check.Command) > 32 {
			return nil, ErrInvalid
		}
		seen[check.ID] = true
		requiredChecks[check.ID] = check.Required()
		if len(check.TrustedPaths) > 128 {
			return nil, ErrInvalid
		}
		for _, name := range check.TrustedPaths {
			if !evidence.Path(name) {
				return nil, ErrInvalid
			}
		}
		for _, part := range check.Command {
			if part == "" || len(part) > 4096 || strings.ContainsRune(part, 0) {
				return nil, ErrInvalid
			}
		}
	}
	for _, id := range required {
		if !requiredChecks[id] {
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
