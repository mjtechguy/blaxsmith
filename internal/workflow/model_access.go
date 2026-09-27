package workflow

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var projectModelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var ErrProjectModelAccessDenied = errors.New("project model access denied")

type ProjectModelAccess struct {
	ID, ProjectID, Provider, Model, ConnectionID, GrantID string
	CreatedAt                                             time.Time
}

func (s *Store) ListProjectModelAccess(ctx context.Context, orgID, projectID string) ([]ProjectModelAccess, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, projectID) {
		return nil, ErrInvalid
	}
	if _, err := s.GetProject(ctx, orgID, projectID); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT m.id,m.project_id,m.provider,m.model,c.id,g.id,m.approved_at
		FROM workflow_project_model_grants m
		JOIN access_grants g ON g.organization_id=m.organization_id::text AND g.id=m.grant_id
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		WHERE m.organization_id=$1 AND m.project_id=$2 AND m.revoked_at IS NULL
		AND g.revoked_at IS NULL AND c.state='active'
		ORDER BY m.provider,m.model`, orgID, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []ProjectModelAccess{}
	for rows.Next() {
		var item ProjectModelAccess
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.Provider, &item.Model,
			&item.ConnectionID, &item.GrantID, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// CreateProjectModelAccessAs commits a provider registration, organization
// connection, encrypted API key, standing grant, and explicit project selection
// in one transaction. It never returns credential material.
func (s *Store) CreateProjectModelAccessAs(ctx context.Context, caller identity.Caller, projectID, provider, model string,
	apiKey []byte, secrets *access.SecretStore) (ProjectModelAccess, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if caller.Role != "owner" && caller.Role != "admin" {
		return ProjectModelAccess{}, ErrProjectModelAccessDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, projectID) || secrets == nil {
		return ProjectModelAccess{}, ErrFenced
	}
	origin := ""
	switch provider {
	case "openai":
		origin = "https://api.openai.com"
	case "anthropic":
		origin = "https://api.anthropic.com"
	}
	if origin == "" || !projectModelName.MatchString(model) || len(apiKey) == 0 || len(apiKey) > 8192 ||
		strings.ContainsAny(string(apiKey), "\r\n\x00") {
		return ProjectModelAccess{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ProjectModelAccess{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockProjectModelAccessAdmin(ctx, tx, caller); err != nil {
		return ProjectModelAccess{}, err
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		caller.OrganizationID, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectModelAccess{}, ErrNotFound
	}
	if err != nil {
		return ProjectModelAccess{}, err
	}
	var providerID, connectionID, grantID string
	err = tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,$2,$3,ARRAY['native_raw'],'active') RETURNING id`,
		caller.OrganizationID, provider, origin).Scan(&providerID)
	if err != nil {
		return ProjectModelAccess{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,gen_random_uuid()::text,'organization',$1,$2,$3,'api_key','active') RETURNING id`,
		caller.OrganizationID, providerID, "unverified-"+provider+"-api-key").Scan(&connectionID)
	if err != nil {
		return ProjectModelAccess{}, err
	}
	if _, err := secrets.RotateTx(ctx, tx, caller.OrganizationID, connectionID, 0, apiKey, nil); err != nil {
		return ProjectModelAccess{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_project_policies
		(organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ($1,$2,1,false,ARRAY['native_raw'])
		ON CONFLICT (organization_id,project_id) DO UPDATE SET
		version=CASE WHEN 'native_raw'=ANY(access_project_policies.delivery_modes)
			THEN access_project_policies.version ELSE access_project_policies.version+1 END,
		delivery_modes=CASE WHEN 'native_raw'=ANY(access_project_policies.delivery_modes)
			THEN access_project_policies.delivery_modes
			ELSE array_append(access_project_policies.delivery_modes,'native_raw') END`,
		caller.OrganizationID, projectID); err != nil {
		return ProjectModelAccess{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ($1,gen_random_uuid()::text,$2,$3,'workload','blaxsmith-dispatcher','model.invoke',$4,'native_raw',$5)
		RETURNING id`, caller.OrganizationID, connectionID, projectID, provider+"/"+model,
		caller.PrincipalID).Scan(&grantID)
	if err != nil {
		return ProjectModelAccess{}, err
	}
	var item ProjectModelAccess
	err = tx.QueryRow(ctx, `INSERT INTO workflow_project_model_grants
		(organization_id,project_id,provider,model,grant_id,grantee_id,approved_by)
		VALUES ($1,$2,$3,$4,$5,'blaxsmith-dispatcher',$6)
		ON CONFLICT (organization_id,project_id,provider,model) WHERE revoked_at IS NULL DO NOTHING
		RETURNING id,project_id,provider,model,approved_at`, caller.OrganizationID, projectID,
		provider, model, grantID, caller.PrincipalID).
		Scan(&item.ID, &item.ProjectID, &item.Provider, &item.Model, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProjectModelAccess{}, ErrConflict
	}
	if err != nil {
		return ProjectModelAccess{}, err
	}
	item.ConnectionID, item.GrantID = connectionID, grantID
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'access.project_model.created',$3)`,
		caller.OrganizationID, caller.PrincipalID, item.ID); err != nil {
		return ProjectModelAccess{}, err
	}
	return item, tx.Commit(ctx)
}

func (s *Store) RevokeProjectModelAccessAs(ctx context.Context, caller identity.Caller, accessID string) error {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrProjectModelAccessDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || !uuidPattern.MatchString(accessID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockProjectModelAccessAdmin(ctx, tx, caller); err != nil {
		return err
	}
	var grantID, connectionID string
	err = tx.QueryRow(ctx, `SELECT g.id,c.id FROM workflow_project_model_grants m
		JOIN access_grants g ON g.organization_id=m.organization_id::text AND g.id=m.grant_id
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		WHERE m.organization_id=$1 AND m.id=$2 FOR UPDATE OF m,g,c`, caller.OrganizationID, accessID).
		Scan(&grantID, &connectionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_project_model_grants SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL`, caller.OrganizationID, accessID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_grants SET revoked_at=COALESCE(revoked_at,clock_timestamp()),
		version=CASE WHEN revoked_at IS NULL THEN version+1 ELSE version END
		WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, grantID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND binding_id IN
		(SELECT id FROM access_bindings WHERE organization_id=$1 AND grant_id=$2)
		AND revoked_at IS NULL`, caller.OrganizationID, grantID); err != nil {
		return err
	}
	var activeGrants int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM access_grants
		WHERE organization_id=$1 AND connection_id=$2 AND revoked_at IS NULL`,
		caller.OrganizationID, connectionID).Scan(&activeGrants); err != nil {
		return err
	}
	if activeGrants == 0 {
		if _, err := tx.Exec(ctx, `UPDATE access_connections SET state='revoked'
			WHERE organization_id=$1 AND id=$2 AND state<>'revoked'`, caller.OrganizationID, connectionID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp()
			WHERE organization_id=$1 AND connection_id=$2 AND revoked_at IS NULL`,
			caller.OrganizationID, connectionID); err != nil {
			return err
		}
	}
	if tag.RowsAffected() > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
			(organization_id,actor_kind,actor_id,action,subject_id)
			VALUES ($1,'principal',$2,'access.project_model.revoked',$3)`,
			caller.OrganizationID, caller.PrincipalID, accessID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func lockProjectModelAccessAdmin(ctx context.Context, tx pgx.Tx, caller identity.Caller) error {
	return lockCallerSession(ctx, tx, caller, false)
}

// lockCallerSession holds the live session and membership; anyRole admits
// every active member rather than only owners and admins.
func lockCallerSession(ctx context.Context, tx pgx.Tx, caller identity.Caller, anyRole bool) error {
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND ($6 OR m.role IN ('owner','admin')) AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (s.credential_kind='service' OR o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires, anyRole).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	return err
}
