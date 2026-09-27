package identity

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

type ServicePrincipal struct {
	ID, ProjectID, Label, State, CreatedBy string
	CreatedAt                              time.Time
}

func requireBrowserAdmin(ctx context.Context, tx pgx.Tx, caller Caller) error {
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return err
	}
	var kind string
	if err := tx.QueryRow(ctx, `SELECT credential_kind FROM identity_sessions WHERE organization_id=$1 AND id=$2 AND NOT email_required`, caller.OrganizationID, caller.SessionID).Scan(&kind); err != nil {
		return err
	}
	if kind != "browser" {
		return ErrUnauthenticated
	}
	return nil
}

func (m *SessionManager) CreateServicePrincipal(ctx context.Context, caller Caller, project, label, key string) (ServicePrincipal, error) {
	var out ServicePrincipal
	if !validUUID(project) || len(strings.TrimSpace(label)) < 1 || len(label) > 100 || len(key) < 1 || len(key) > 128 || strings.ContainsRune(label+key, 0) {
		return out, ErrUnauthenticated
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return out, err
	}
	defer tx.Rollback(ctx)
	if err = requireBrowserAdmin(ctx, tx, caller); err != nil {
		return out, err
	}
	var oldLabel string
	err = tx.QueryRow(ctx, `SELECT sp.principal_id,sp.label,CASE WHEN p.state='active' AND m.state='active' THEN 'active' ELSE 'disabled' END,sp.created_at FROM identity_service_principals sp JOIN identity_principals p ON p.id=sp.principal_id JOIN identity_memberships m ON m.organization_id=sp.organization_id AND m.principal_id=sp.principal_id WHERE sp.organization_id=$1 AND sp.project_id=$2 AND sp.created_by=$3 AND sp.request_key=$4`, caller.OrganizationID, project, caller.PrincipalID, key).Scan(&out.ID, &oldLabel, &out.State, &out.CreatedAt)
	out.ProjectID, out.Label, out.CreatedBy = project, label, caller.PrincipalID
	if err == nil {
		if oldLabel != label {
			return out, ErrUnauthenticated
		}
		return out, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM identity_service_principals WHERE organization_id=$1 AND project_id=$2`, caller.OrganizationID, project).Scan(&count); err != nil {
		return out, err
	}
	if count >= 100 {
		return out, errors.New("project service identity limit reached")
	}
	if err = tx.QueryRow(ctx, `WITH id AS (SELECT gen_random_uuid() AS value) INSERT INTO identity_principals(id,username,kind,display_name) SELECT value,'service-'||value::text,'service',$1 FROM id RETURNING id,state,created_at`, label).Scan(&out.ID, &out.State, &out.CreatedAt); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_memberships(organization_id,principal_id,role) VALUES($1,$2,'member')`, caller.OrganizationID, out.ID); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_service_principals(organization_id,principal_id,project_id,label,request_key,created_by) VALUES($1,$2,$3,$4,$5,$6)`, caller.OrganizationID, out.ID, project, label, key, caller.PrincipalID); err != nil {
		return out, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,'identity.service.created',$3)`, caller.OrganizationID, caller.PrincipalID, out.ID); err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (m *SessionManager) ListServicePrincipals(ctx context.Context, caller Caller, project string) ([]ServicePrincipal, error) {
	if !validUUID(project) || userAdminRole(caller) != nil {
		return nil, ErrUnauthenticated
	}
	rows, err := m.db.Query(tenant.Org(ctx, caller.OrganizationID), `SELECT sp.principal_id,sp.project_id,sp.label,CASE WHEN p.state='active' AND m.state='active' THEN 'active' ELSE 'disabled' END,sp.created_by,sp.created_at FROM identity_service_principals sp JOIN identity_principals p ON p.id=sp.principal_id JOIN identity_memberships m ON m.organization_id=sp.organization_id AND m.principal_id=sp.principal_id WHERE sp.organization_id=$1 AND sp.project_id=$2 ORDER BY sp.created_at DESC,sp.principal_id LIMIT 100`, caller.OrganizationID, project)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ServicePrincipal, error) {
		var s ServicePrincipal
		err := row.Scan(&s.ID, &s.ProjectID, &s.Label, &s.State, &s.CreatedBy, &s.CreatedAt)
		return s, err
	})
}

func (m *SessionManager) DisableServicePrincipal(ctx context.Context, caller Caller, id string) error {
	if !validUUID(id) {
		return ErrUnauthenticated
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = requireBrowserAdmin(ctx, tx, caller); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE identity_memberships m SET state='disabled' FROM identity_service_principals sp WHERE sp.organization_id=m.organization_id AND sp.principal_id=m.principal_id AND sp.organization_id=$1 AND sp.principal_id=$2`, caller.OrganizationID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrUnauthenticated
	}
	if _, err = tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE organization_id=$1 AND principal_id=$2`, caller.OrganizationID, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,'identity.service.disabled',$3)`, caller.OrganizationID, caller.PrincipalID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
