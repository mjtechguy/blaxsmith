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
	SelectionID string
	GrantID     string
	GranteeID   string // First proof: organization-owned workload only.
}

func (s *Store) GetProjectModelGrant(ctx context.Context, orgID, projectID, provider, model string) (ProjectModelGrant, error) {
	if !ids(orgID, projectID) || provider == "" || model == "" {
		return ProjectModelGrant{}, ErrInvalid
	}
	var selection ProjectModelGrant
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
func LockDispatchSelections(ctx context.Context, tx pgx.Tx, orgID, projectID, runtimeApprovalID, grantSelectionID string) error {
	if tx == nil || !ids(orgID, projectID, runtimeApprovalID, grantSelectionID) {
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
