package access

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// Resource kinds an organization can grant through access_resource_grants.
const (
	ResourceConnection = "connection"
	ResourceRecipe     = "recipe"
	ResourceExtension  = "extension"
)

var ErrResourceGrantInvalid = errors.New("invalid resource grant")

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func validResource(kind, id string) bool {
	return (kind == ResourceConnection || kind == ResourceRecipe || kind == ResourceExtension) && id != "" && len(id) <= 200
}

// CanUse reports whether principal may use one organization-level resource,
// optionally in the context of projectID ("" for none). It answers only the
// resource-grant question; the caller still checks the operation itself (for
// example that the principal may launch runs in that project) and that
// resourceID names a live resource in principal.OrganizationID.
//
// It is true when principal is an active, non-viewer member of its
// organization (re-read under tx, not taken from the token) and an unrevoked
// grant in that same organization for (resourceKind, resourceID) names
//   - projectID, when projectID is non-empty;
//   - principal.PrincipalID; or
//   - a role at or below the member's current role (owner > admin > member).
//
// Owners and admins have no implicit use: grant the "admin" role if they need
// it. Every lookup is scoped to principal.OrganizationID, so a grant, project,
// or resource ID from another organization never matches. Matching grant and
// membership rows are locked FOR SHARE, so a concurrent revocation or role
// change waits for tx to finish rather than racing the use it authorizes.
// Unknown kinds and malformed IDs deny (false, nil). resourceKind is
// ResourceConnection ("connection"), ResourceRecipe ("recipe"), or
// ResourceExtension ("extension").
func CanUse(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID, resourceKind, resourceID string) (bool, error) {
	_, ok, err := MatchingGrant(ctx, tx, principal, projectID, resourceKind, resourceID)
	return ok, err
}

// GrantMatch is the grant that satisfied CanUse. Via is project, user, or
// role; Role is the grant's minimum role for a role grant.
type GrantMatch struct{ GrantID, Via, Role string }

// MatchingGrant is CanUse's single implementation: it returns the grant that
// authorizes the use, preferring a project grant, then a user grant, then the
// narrowest role grant, so an explanation names the most specific reason.
func MatchingGrant(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID, resourceKind, resourceID string) (GrantMatch, bool, error) {
	if tx == nil {
		return GrantMatch{}, false, ErrDenied
	}
	if !uuidPattern.MatchString(principal.OrganizationID) || !uuidPattern.MatchString(principal.PrincipalID) ||
		(projectID != "" && !uuidPattern.MatchString(projectID)) || !validResource(resourceKind, resourceID) {
		return GrantMatch{}, false, nil
	}
	var m GrantMatch
	err := tx.QueryRow(ctx, `SELECT g.id,
		CASE WHEN g.grantee_project_id IS NOT NULL THEN 'project' WHEN g.grantee_principal_id IS NOT NULL THEN 'user' ELSE 'role' END,
		COALESCE(g.grantee_role,'')
		FROM identity_memberships m
		JOIN identity_principals p ON p.id=m.principal_id
		JOIN access_resource_grants g ON g.organization_id=m.organization_id
		WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.state='active' AND p.state='active' AND m.role<>'viewer'
		AND g.resource_kind=$4 AND g.resource_id=$5 AND g.revoked_at IS NULL
		AND (g.grantee_principal_id=m.principal_id
			OR g.grantee_project_id=NULLIF($3,'')::uuid
			OR array_position(ARRAY['member','admin','owner'],m.role)>=array_position(ARRAY['member','admin','owner'],g.grantee_role))
		ORDER BY g.grantee_project_id IS NULL, g.grantee_principal_id IS NULL,
			array_position(ARRAY['member','admin','owner'],g.grantee_role) DESC NULLS LAST, g.created_at, g.id
		LIMIT 1 FOR SHARE OF m,g`, principal.OrganizationID, principal.PrincipalID, projectID, resourceKind, resourceID).
		Scan(&m.GrantID, &m.Via, &m.Role)
	if errors.Is(err, pgx.ErrNoRows) {
		return GrantMatch{}, false, nil
	}
	return m, err == nil, err
}

// Grantee names exactly one recipient of a resource grant: a project in the
// organization, a member, or a minimum role ("member", "admin", or "owner").
type Grantee struct{ ProjectID, PrincipalID, Role string }

// GrantResource records a grant and audits it. caller must currently be an
// active owner or admin of its organization; the caller's session is the
// transport's concern. Granting the same live grant twice returns the
// existing grant's ID.
func GrantResource(ctx context.Context, tx pgx.Tx, caller identity.Caller, resourceKind, resourceID string, to Grantee) (string, error) {
	set := 0
	for _, value := range []string{to.ProjectID, to.PrincipalID, to.Role} {
		if value != "" {
			set++
		}
	}
	if tx == nil || set != 1 || !validResource(resourceKind, resourceID) ||
		(to.ProjectID != "" && !uuidPattern.MatchString(to.ProjectID)) ||
		(to.PrincipalID != "" && !uuidPattern.MatchString(to.PrincipalID)) ||
		(to.Role != "" && to.Role != "member" && to.Role != "admin" && to.Role != "owner") {
		return "", ErrResourceGrantInvalid
	}
	if err := lockResourceAdmin(ctx, tx, caller); err != nil {
		return "", err
	}
	var id string
	err := tx.QueryRow(ctx, `SELECT id FROM access_resource_grants WHERE organization_id=$1 AND resource_kind=$2
		AND resource_id=$3 AND revoked_at IS NULL
		AND COALESCE(grantee_project_id::text,grantee_principal_id::text,grantee_role)=$4`,
		caller.OrganizationID, resourceKind, resourceID, to.ProjectID+to.PrincipalID+to.Role).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	// Check the grantee first so a cross-organization ID is a clean error,
	// not a foreign-key failure that aborts the caller's transaction.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT CASE WHEN $2<>'' THEN EXISTS(SELECT 1 FROM workflow_projects
			WHERE organization_id=$1 AND id=NULLIF($2,'')::uuid)
		WHEN $3<>'' THEN EXISTS(SELECT 1 FROM identity_memberships WHERE organization_id=$1 AND principal_id=NULLIF($3,'')::uuid)
		ELSE true END`, caller.OrganizationID, to.ProjectID, to.PrincipalID).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		return "", ErrResourceGrantInvalid
	}
	if err := tx.QueryRow(ctx, `INSERT INTO access_resource_grants
		(organization_id,resource_kind,resource_id,grantee_project_id,grantee_principal_id,grantee_role,granted_by)
		VALUES ($1,$2,$3,NULLIF($4,'')::uuid,NULLIF($5,'')::uuid,NULLIF($6,''),$7) RETURNING id`,
		caller.OrganizationID, resourceKind, resourceID, to.ProjectID, to.PrincipalID, to.Role, caller.PrincipalID).Scan(&id); err != nil {
		return "", err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,project_id,detail)
		VALUES ($1,'principal',$2,'access.resource_grant.created',$3,NULLIF($4,'')::uuid,
		jsonb_build_object('resource_kind',$5::text,'resource_id',$6::text))`,
		caller.OrganizationID, caller.PrincipalID, id, to.ProjectID, resourceKind, resourceID)
	return id, err
}

// RevokeResourceGrant revokes one live grant in caller's organization.
func RevokeResourceGrant(ctx context.Context, tx pgx.Tx, caller identity.Caller, grantID string) error {
	if tx == nil || !uuidPattern.MatchString(grantID) {
		return ErrResourceGrantInvalid
	}
	if err := lockResourceAdmin(ctx, tx, caller); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE access_resource_grants SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL`, caller.OrganizationID, grantID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrResourceGrantInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'access.resource_grant.revoked',$3)`, caller.OrganizationID, caller.PrincipalID, grantID)
	return err
}

func lockResourceAdmin(ctx context.Context, tx pgx.Tx, caller identity.Caller) error {
	if !uuidPattern.MatchString(caller.OrganizationID) || !uuidPattern.MatchString(caller.PrincipalID) {
		return ErrDenied
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM identity_memberships m JOIN identity_principals p ON p.id=m.principal_id
		WHERE m.organization_id=$1 AND m.principal_id=$2 AND m.state='active' AND p.state='active'
		AND m.role IN ('owner','admin') FOR SHARE OF m`, caller.OrganizationID, caller.PrincipalID).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}
