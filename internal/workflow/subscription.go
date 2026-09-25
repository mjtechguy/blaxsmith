package workflow

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// ErrSubscriptionPolicy means the project has not enabled subscription logins.
var ErrSubscriptionPolicy = errors.New("project policy does not permit subscription logins; ask a project admin")

type SubscriptionConnection struct {
	ConnectionID, ProjectID, Provider, Model, AccountID, State string
	CreatedAt                                                  time.Time
}

// ListSubscriptionConnections returns only the caller's own logins and never
// secret material.
func (s *Store) ListSubscriptionConnections(ctx context.Context, caller identity.Caller, projectID string) ([]SubscriptionConnection, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, projectID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,g.project_id,g.resource,c.external_account_id,
		CASE WHEN c.state<>'active' OR g.revoked_at IS NOT NULL THEN 'revoked'
		     WHEN o.reconnect_reason IS NOT NULL THEN 'reconnect_required' ELSE 'active' END,
		c.created_at
		FROM access_connections c
		JOIN access_grants g ON g.organization_id=c.organization_id AND g.connection_id=c.id
		JOIN access_oauth_sessions o ON o.organization_id=c.organization_id AND o.connection_id=c.id
		WHERE c.organization_id=$1 AND c.owner_kind='user' AND c.owner_id=$2 AND c.auth_method=$3
		AND g.project_id=$4 AND g.grantee_kind='user' AND g.grantee_id=$2
		ORDER BY c.created_at DESC LIMIT 100`,
		caller.OrganizationID, caller.PrincipalID, access.CodexSubscriptionAuth, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []SubscriptionConnection{}
	for rows.Next() {
		var item SubscriptionConnection
		var resource string
		if err := rows.Scan(&item.ConnectionID, &item.ProjectID, &resource, &item.AccountID,
			&item.State, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.Provider, item.Model, _ = strings.Cut(resource, "/")
		items = append(items, item)
	}
	return items, rows.Err()
}

// CreateSubscriptionConnectionAs stores the caller's own Codex login and a
// grant usable only by the caller's attempts. Owners and admins enable the
// project's oauth_access policy as they connect; other members need it enabled.
func (s *Store) CreateSubscriptionConnectionAs(ctx context.Context, caller identity.Caller, projectID, provider, model string,
	credential []byte, secrets *access.SecretStore) (SubscriptionConnection, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if provider == "anthropic" {
		return SubscriptionConnection{}, access.ErrClaudeSubscriptionDisabled
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, projectID) || secrets == nil {
		return SubscriptionConnection{}, ErrFenced
	}
	if provider != "openai" || !projectModelName.MatchString(model) || len(credential) == 0 {
		return SubscriptionConnection{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return SubscriptionConnection{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return SubscriptionConnection{}, err
	}
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		caller.OrganizationID, projectID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubscriptionConnection{}, ErrNotFound
	}
	if err != nil {
		return SubscriptionConnection{}, err
	}
	if caller.Role == "owner" || caller.Role == "admin" {
		_, err = tx.Exec(ctx, `INSERT INTO access_project_policies
			(organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ($1,$2,1,false,ARRAY['oauth_access'])
			ON CONFLICT (organization_id,project_id) DO UPDATE SET
			version=CASE WHEN 'oauth_access'=ANY(access_project_policies.delivery_modes)
				THEN access_project_policies.version ELSE access_project_policies.version+1 END,
			delivery_modes=CASE WHEN 'oauth_access'=ANY(access_project_policies.delivery_modes)
				THEN access_project_policies.delivery_modes
				ELSE array_append(access_project_policies.delivery_modes,'oauth_access') END`,
			caller.OrganizationID, projectID)
	} else {
		err = tx.QueryRow(ctx, `SELECT true FROM access_project_policies WHERE organization_id=$1
			AND project_id=$2 AND 'oauth_access'=ANY(delivery_modes) FOR SHARE`,
			caller.OrganizationID, projectID).Scan(&exists)
		if errors.Is(err, pgx.ErrNoRows) {
			return SubscriptionConnection{}, ErrSubscriptionPolicy
		}
	}
	if err != nil {
		return SubscriptionConnection{}, err
	}
	var providerID string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,'openai','https://api.openai.com',ARRAY['oauth_access'],'active')
		RETURNING id`, caller.OrganizationID).Scan(&providerID); err != nil {
		return SubscriptionConnection{}, err
	}
	connectionID, account, err := access.CreateCodexConnection(ctx, tx, secrets, caller.OrganizationID,
		caller.PrincipalID, providerID, credential)
	if errors.Is(err, access.ErrDenied) {
		return SubscriptionConnection{}, ErrInvalid
	}
	if err != nil {
		return SubscriptionConnection{}, err
	}
	item := SubscriptionConnection{ConnectionID: connectionID, ProjectID: projectID, Provider: provider,
		Model: model, AccountID: account.AccountID, State: "active"}
	if err := tx.QueryRow(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ($1,gen_random_uuid()::text,$2,$3,'user',$4,'model.invoke',$5,'oauth_access',$4)
		RETURNING created_at`, caller.OrganizationID, connectionID, projectID, caller.PrincipalID,
		provider+"/"+model).Scan(&item.CreatedAt); err != nil {
		return SubscriptionConnection{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'access.subscription.connected',$3)`,
		caller.OrganizationID, caller.PrincipalID, connectionID); err != nil {
		return SubscriptionConnection{}, err
	}
	return item, tx.Commit(ctx)
}

// RevokeSubscriptionConnectionAs lets the owner (or an org owner/admin, who
// can disable but never read it) stop every grant and lease of one login.
func (s *Store) RevokeSubscriptionConnectionAs(ctx context.Context, caller identity.Caller, connectionID string) error {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || !uuidPattern.MatchString(connectionID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return err
	}
	var ownerID string
	err = tx.QueryRow(ctx, `SELECT owner_id FROM access_connections WHERE organization_id=$1 AND id=$2
		AND owner_kind='user' AND auth_method=$3 FOR UPDATE`, caller.OrganizationID, connectionID,
		access.CodexSubscriptionAuth).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if ownerID != caller.PrincipalID && caller.Role != "owner" && caller.Role != "admin" {
		return ErrNotFound
	}
	for _, statement := range []string{
		`UPDATE access_connections SET state='revoked' WHERE organization_id=$1 AND id=$2`,
		`UPDATE access_grants SET revoked_at=clock_timestamp(),version=version+1
			WHERE organization_id=$1 AND connection_id=$2 AND revoked_at IS NULL`,
		`UPDATE access_leases SET revoked_at=clock_timestamp()
			WHERE organization_id=$1 AND connection_id=$2 AND revoked_at IS NULL`,
	} {
		if _, err := tx.Exec(ctx, statement, caller.OrganizationID, connectionID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'access.subscription.revoked',$3)`,
		caller.OrganizationID, caller.PrincipalID, connectionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
