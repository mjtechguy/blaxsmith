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

func (s *Store) SetProjectVerificationAs(ctx context.Context, caller identity.Caller, projectID string, expectedVersion int64, policy VerificationPolicy) (ProjectVerification, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, projectID) ||
		(caller.Role != "owner" && caller.Role != "admin") {
		return ProjectVerification{}, ErrProjectVerificationDenied
	}
	if expectedVersion < 0 {
		return ProjectVerification{}, ErrInvalid
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
	err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR UPDATE`,
		caller.OrganizationID, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectVerification{}, ErrNotFound
	}
	if err != nil {
		return ProjectVerification{}, err
	}
	var version int64
	err = tx.QueryRow(ctx, `SELECT version FROM workflow_project_verification WHERE organization_id=$1 AND project_id=$2`, caller.OrganizationID, projectID).Scan(&version)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ProjectVerification{}, err
	}
	if version != expectedVersion {
		return ProjectVerification{}, ErrConflict
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
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_verification_history (organization_id,project_id,version,policy_json,updated_at,principal_id) VALUES ($1,$2,$3,$4,$5,$6)`, caller.OrganizationID, projectID, result.Version, data, result.UpdatedAt, caller.PrincipalID); err != nil {
		return ProjectVerification{}, err
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

// BeforeVersion is an exclusive revision cursor; zero starts at the newest.
func (s *Store) ListProjectVerificationHistory(ctx context.Context, orgID, projectID string, beforeVersion int64) ([]ProjectVerification, error) {
	if !ids(orgID, projectID) || beforeVersion < 0 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(tenant.Org(ctx, orgID), `SELECT policy_json,version,updated_at FROM workflow_verification_history WHERE organization_id=$1 AND project_id=$2 AND ($3::bigint=0 OR version<$3) ORDER BY version DESC LIMIT 50`, orgID, projectID, beforeVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []ProjectVerification{}
	for rows.Next() {
		var item ProjectVerification
		var data []byte
		if err := rows.Scan(&data, &item.Version, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &item.Policy); err != nil {
			return nil, err
		}
		results = append(results, item)
	}
	return results, rows.Err()
}
