package workflow

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

var ErrProjectDenied = errors.New("project creation denied")

// CreateProjectAs rechecks the live session under locks and records the
// project and principal audit event in one transaction.
func (s *Store) CreateProjectAs(ctx context.Context, caller identity.Caller, slug, name string) (string, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) ||
		!slugPattern.MatchString(slug) || strings.TrimSpace(name) == "" || len(name) > 160 {
		return "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
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
		return "", ErrProjectDenied
	}
	if err != nil {
		return "", err
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO workflow_projects (organization_id,slug,name)
		VALUES ($1,$2,$3) RETURNING id`, caller.OrganizationID, slug, name).Scan(&id); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'workflow.project.created',$3)`, caller.OrganizationID, caller.PrincipalID, id); err != nil {
		return "", err
	}
	return id, tx.Commit(ctx)
}
