package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// Organization recipe grants are lane U resource grants (access_resource_grants,
// kind "recipe"), managed exactly like lane C's connection grants and returned
// in the same ConnectionGrant shape. Project recipes are never granted.

const recipeGrantColumns = `SELECT g.id::text,COALESCE(g.grantee_project_id::text,''),COALESCE(pr.name,''),
	CASE WHEN g.grantee_project_id IS NOT NULL THEN 'project' WHEN g.grantee_principal_id IS NOT NULL THEN 'user' ELSE 'role' END,
	COALESCE(g.grantee_principal_id::text,g.grantee_role,''),COALESCE(identity_principal_label(u.display_name,u.email,u.username),''),g.created_at
	FROM access_resource_grants g
	LEFT JOIN workflow_projects pr ON pr.organization_id=g.organization_id AND pr.id=g.grantee_project_id
	LEFT JOIN identity_principals u ON u.id=g.grantee_principal_id
	WHERE g.organization_id=$1::uuid AND g.resource_kind='recipe' AND g.revoked_at IS NULL`

func scanRecipeGrant(row pgx.Row) (ConnectionGrant, error) {
	var g ConnectionGrant
	err := row.Scan(&g.ID, &g.ProjectID, &g.ProjectName, &g.GranteeKind, &g.GranteeID, &g.GranteeName, &g.CreatedAt)
	return g, err
}

// RecipeGrants lists live grants on an organization recipe. Only organization
// owners/admins see grants; others get none.
func (s *Store) RecipeGrants(ctx context.Context, caller identity.Caller, recipeID string) ([]ConnectionGrant, error) {
	if !ids(caller.OrganizationID, recipeID) {
		return nil, ErrInvalid
	}
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, nil
	}
	rows, err := s.pool.Query(ctx, recipeGrantColumns+` AND g.resource_id=$2 ORDER BY g.created_at,g.id LIMIT 2000`,
		caller.OrganizationID, recipeID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConnectionGrant, error) { return scanRecipeGrant(row) })
}

// GrantRecipeAs grants an organization recipe to a project, a member, or a
// minimum role (member, admin, owner). Organization owners/admins only;
// access.GrantResource audits it.
func (s *Store) GrantRecipeAs(ctx context.Context, caller identity.Caller, recipeID, projectID, granteeKind, granteeID string) (ConnectionGrant, error) {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ConnectionGrant{}, ErrRecipeDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, recipeID) {
		return ConnectionGrant{}, ErrInvalid
	}
	var to access.Grantee
	switch granteeKind {
	case "project":
		to.ProjectID = projectID
	case "user":
		to.PrincipalID = granteeID
	case "role":
		to.Role = granteeID
	default:
		return ConnectionGrant{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ConnectionGrant{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return ConnectionGrant{}, err
	}
	recipeProject, err := lockRecipe(ctx, tx, caller.OrganizationID, recipeID)
	if err != nil {
		return ConnectionGrant{}, err
	}
	if recipeProject != "" {
		return ConnectionGrant{}, ErrInvalid // Project recipes belong to their project.
	}
	id, err := access.GrantResource(ctx, tx, caller, access.ResourceRecipe, recipeID, to)
	if errors.Is(err, access.ErrResourceGrantInvalid) {
		return ConnectionGrant{}, ErrInvalid
	}
	if errors.Is(err, access.ErrDenied) {
		return ConnectionGrant{}, ErrRecipeDenied
	}
	if err != nil {
		return ConnectionGrant{}, err
	}
	grant, err := scanRecipeGrant(tx.QueryRow(ctx, recipeGrantColumns+` AND g.id=$2::uuid`, caller.OrganizationID, id))
	if err != nil {
		return ConnectionGrant{}, err
	}
	return grant, tx.Commit(ctx)
}

// RevokeRecipeGrantAs revokes one live organization recipe grant; runs
// already launched keep the bytes they froze.
func (s *Store) RevokeRecipeGrantAs(ctx context.Context, caller identity.Caller, grantID string) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrRecipeDenied
	}
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, grantID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return err
	}
	var found bool
	err = tx.QueryRow(ctx, `SELECT true FROM access_resource_grants WHERE organization_id=$1::uuid AND id=$2::uuid
		AND resource_kind='recipe' AND revoked_at IS NULL FOR UPDATE`, caller.OrganizationID, grantID).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := access.RevokeResourceGrant(ctx, tx, caller, grantID); errors.Is(err, access.ErrDenied) {
		return ErrRecipeDenied
	} else if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
