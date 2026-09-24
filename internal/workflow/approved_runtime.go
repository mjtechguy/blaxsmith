package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
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

// ResolveModelGrant picks the grant a run's attempt binds for provider/model.
// A personal (user-owned) grant is honoured only when the run's initiator
// owns that connection and is its grantee; otherwise the project's workload
// selection applies, as before. initiator is empty for runs without one.
func (s *Store) ResolveModelGrant(ctx context.Context, orgID, projectID, initiator, provider, model string) (ProjectModelGrant, error) {
	if initiator != "" && ids(initiator) {
		var grant ProjectModelGrant
		err := s.pool.QueryRow(ctx, `SELECT g.id,g.grantee_id FROM access_grants g
			JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
			WHERE g.organization_id=$1 AND g.project_id=$2 AND g.capability='model.invoke' AND g.resource=$3
			AND g.grantee_kind='user' AND g.grantee_id=$4 AND c.owner_kind='user' AND c.owner_id=$4
			AND g.revoked_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>clock_timestamp()) AND c.state='active'
			ORDER BY g.created_at DESC,g.id LIMIT 1`, orgID, projectID, provider+"/"+model, initiator).
			Scan(&grant.GrantID, &grant.GranteeID)
		if err == nil {
			grant.GranteeKind = "user"
			return grant, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return ProjectModelGrant{}, err
		}
	}
	return s.GetProjectModelGrant(ctx, orgID, projectID, provider, model)
}

// RunInitiator returns the principal who launched runID, or "".
func (s *Store) RunInitiator(ctx context.Context, orgID, runID string) (string, error) {
	var initiator *string
	err := s.pool.QueryRow(ctx, `SELECT initiator_principal_id FROM workflow_runs WHERE organization_id=$1 AND id=$2`,
		orgID, runID).Scan(&initiator)
	if err != nil || initiator == nil {
		return "", err
	}
	return *initiator, nil
}

func (s *Store) GetProjectModelGrant(ctx context.Context, orgID, projectID, provider, model string) (ProjectModelGrant, error) {
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
