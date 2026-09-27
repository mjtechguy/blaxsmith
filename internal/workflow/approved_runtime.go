package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

// ApprovedToolRuntime is read from an operator-managed approval record, never
// from Git or the public release catalog.
type ApprovedToolRuntime struct {
	ApprovalID        string
	Runtime           tooladapter.Runtime
	WorkerPool        string
	MaxTimeoutSeconds int
	MaxOutputBytes    int
}

type ProjectModelGrant struct {
	SelectionID string // Empty for a personal grant, which has no project selection.
	GrantID     string
	GranteeKind string // workload, or user for the run initiator's own connection.
	GranteeID   string
}

// One grant predicate is shared by catalog discovery and execution. Project
// workload selections and owned personal grants are the only candidates.
const modelGrantCandidates = `FROM access_grants g
 JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
 JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
 LEFT JOIN workflow_project_model_grants w ON w.organization_id::text=g.organization_id AND w.project_id::text=g.project_id AND w.grant_id=g.id AND w.grantee_id=g.grantee_id AND g.resource=w.provider||'/'||w.model AND w.revoked_at IS NULL
 WHERE g.organization_id=$1 AND g.project_id=$2 AND g.capability='model.invoke'
 AND g.revoked_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp()) AND c.state='active' AND p.state='active'
 AND ((g.grantee_kind='user' AND g.grantee_id=$3 AND c.owner_kind='user' AND c.owner_id=$3)
 OR (g.grantee_kind='workload' AND w.id IS NOT NULL AND c.owner_kind IN ('organization','project')))`

// A pinned connection never falls back to another billing account. Without a
// pin, an owned personal grant takes precedence over the project selection.
func (s *Store) ResolveModelGrant(ctx context.Context, orgID, projectID, initiator, provider, model, connection string) (ProjectModelGrant, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, projectID) || (initiator != "" && !ids(initiator)) || provider == "" || model == "" || len(connection) > 64 {
		return ProjectModelGrant{}, ErrInvalid
	}
	var grant ProjectModelGrant
	err := s.pool.QueryRow(ctx, `SELECT CASE WHEN g.grantee_kind='workload' THEN w.id::text ELSE '' END,g.id,g.grantee_kind,g.grantee_id `+modelGrantCandidates+`
 AND g.resource=$4 AND p.provider_kind=$5 AND ($6='' OR c.id=$6)
 ORDER BY g.grantee_kind='user' DESC,g.created_at DESC,g.id LIMIT 1`, orgID, projectID, initiator, provider+"/"+model, provider, connection).Scan(&grant.SelectionID, &grant.GrantID, &grant.GranteeKind, &grant.GranteeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return grant, ErrNotFound
	}
	return grant, err
}

// RunInitiator returns the principal who launched runID, or "".
func (s *Store) RunInitiator(ctx context.Context, orgID, runID string) (string, error) {
	ctx = tenant.Org(ctx, orgID)
	var initiator *string
	err := s.pool.QueryRow(ctx, `SELECT initiator_principal_id FROM workflow_runs WHERE organization_id=$1 AND id=$2`,
		orgID, runID).Scan(&initiator)
	if err != nil || initiator == nil {
		return "", err
	}
	return *initiator, nil
}

func (s *Store) GetProjectModelGrant(ctx context.Context, orgID, projectID, provider, model string) (ProjectModelGrant, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, projectID) || provider == "" || model == "" {
		return ProjectModelGrant{}, ErrInvalid
	}
	var selection ProjectModelGrant
	selection.GranteeKind = "workload"
	err := s.pool.QueryRow(ctx, `SELECT id,grant_id,grantee_id FROM workflow_project_model_grants
		WHERE organization_id=$1 AND project_id=$2 AND provider=$3 AND model=$4 AND revoked_at IS NULL`,
		orgID, projectID, provider, model).
		Scan(&selection.SelectionID, &selection.GrantID, &selection.GranteeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectModelGrant{}, ErrNotFound
	}
	return selection, err
}

// LockDispatchSelections rechecks both immutable approval identities in the
// same transaction that reserves the attempt and binds model authority.
// An empty grantSelectionID is a personal grant: BindModelInvoke locks and
// rechecks that grant row itself.
func LockDispatchSelections(ctx context.Context, tx pgx.Tx, orgID, projectID, runtimeApprovalID, grantSelectionID string) error {
	if tx == nil || !ids(orgID, projectID, runtimeApprovalID) || (grantSelectionID != "" && !ids(grantSelectionID)) {
		return ErrInvalid
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM workflow_tool_runtime_approvals
		WHERE id=$1 AND organization_id=$2 AND revoked_at IS NULL FOR SHARE`, runtimeApprovalID, orgID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if grantSelectionID == "" {
		return nil
	}
	err = tx.QueryRow(ctx, `SELECT id FROM workflow_project_model_grants
		WHERE id=$1 AND organization_id=$2 AND project_id=$3 AND revoked_at IS NULL FOR SHARE`,
		grantSelectionID, orgID, projectID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrConflict
	}
	return err
}

func (s *Store) GetApprovedToolRuntime(ctx context.Context, orgID, harness, model, effort string) (ApprovedToolRuntime, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID) || harness == "" || model == "" || effort == "" {
		return ApprovedToolRuntime{}, ErrInvalid
	}
	var approval ApprovedToolRuntime
	err := s.pool.QueryRow(ctx, `SELECT id,image,binary_path,binary_sha256,version,worker_pool,
		max_timeout_seconds,max_output_bytes FROM workflow_tool_runtime_approvals
		WHERE organization_id=$1 AND harness=$2 AND model=$3 AND effort=$4 AND revoked_at IS NULL`,
		orgID, harness, model, effort).
		Scan(&approval.ApprovalID, &approval.Runtime.Image, &approval.Runtime.Binary,
			&approval.Runtime.BinarySHA256, &approval.Runtime.Version, &approval.WorkerPool,
			&approval.MaxTimeoutSeconds, &approval.MaxOutputBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return ApprovedToolRuntime{}, ErrNotFound
	}
	if err != nil {
		return ApprovedToolRuntime{}, err
	}
	approval.Runtime.Harness = harness
	approval.Runtime.Supported = []tooladapter.ModelEffort{{Model: model, Effort: effort}}
	return approval, nil
}
