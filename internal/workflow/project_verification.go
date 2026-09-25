package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrProjectVerificationDenied = errors.New("project verification change denied")

type ProjectVerification struct {
	Policy    VerificationPolicy
	Version   int64
	UpdatedAt time.Time
}

func (s *Store) GetProjectVerification(ctx context.Context, orgID, projectID string) (ProjectVerification, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, projectID) {
		return ProjectVerification{}, ErrInvalid
	}
	var result ProjectVerification
	var data []byte
	err := s.pool.QueryRow(ctx, `SELECT policy_json,version,updated_at FROM workflow_project_verification
		WHERE organization_id=$1 AND project_id=$2`, orgID, projectID).Scan(&data, &result.Version, &result.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectVerification{}, ErrNotFound
	}
	if err != nil {
		return ProjectVerification{}, err
	}
	if err := json.Unmarshal(data, &result.Policy); err != nil {
		return ProjectVerification{}, fmt.Errorf("decode verification policy: %w", err)
	}
	return result, nil
}

func (s *Store) SetProjectVerificationAs(ctx context.Context, caller identity.Caller, projectID string, policy VerificationPolicy) (ProjectVerification, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, projectID) ||
		(caller.Role != "owner" && caller.Role != "admin") {
		return ProjectVerification{}, ErrProjectVerificationDenied
	}
	data, err := validateVerification(policy, nil)
	if err != nil {
		return ProjectVerification{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectVerification{}, err
	}
	defer tx.Rollback(ctx)
	var role string
	err = tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND m.role IN ('owner','admin') AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectVerification{}, ErrProjectVerificationDenied
	}
	if err != nil {
		return ProjectVerification{}, err
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		caller.OrganizationID, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectVerification{}, ErrNotFound
	}
	if err != nil {
		return ProjectVerification{}, err
	}
	var result ProjectVerification
	err = tx.QueryRow(ctx, `INSERT INTO workflow_project_verification
		(organization_id,project_id,policy_json) VALUES ($1,$2,$3)
		ON CONFLICT (organization_id,project_id) DO UPDATE
		SET policy_json=EXCLUDED.policy_json,version=workflow_project_verification.version+1,
		updated_at=clock_timestamp()
		RETURNING version,updated_at`, caller.OrganizationID, projectID, data).Scan(&result.Version, &result.UpdatedAt)
	if err != nil {
		return ProjectVerification{}, fmt.Errorf("set project verification: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'workflow.project_verification.set',$3)`,
		caller.OrganizationID, caller.PrincipalID, projectID); err != nil {
		return ProjectVerification{}, err
	}
	result.Policy = policy
	return result, tx.Commit(ctx)
}
