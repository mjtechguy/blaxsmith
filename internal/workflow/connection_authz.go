package workflow

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// ConnectionAuthorizer is the single seam for connection use and project
// administration decisions. CanUse has lane U's signature and is lane U's
// access.CanUse (organization resource grants); CanAdministerProject is this
// lane's until project roles exist.
type ConnectionAuthorizer interface {
	CanUse(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID, resourceKind, resourceID string) (bool, error)
	CanAdministerProject(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID string) (bool, error)
}

type grantAuthorizer struct{}

func (grantAuthorizer) CanUse(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID, resourceKind, resourceID string) (bool, error) {
	return access.CanUse(ctx, tx, principal, projectID, resourceKind, resourceID)
}

// CanAdministerProject: organization owners/admins, or a member recorded in
// workflow_project_admins (the project's creator). Temporary until lane U
// adds project roles; viewers never.
func (grantAuthorizer) CanAdministerProject(ctx context.Context, tx pgx.Tx, principal identity.Caller, projectID string) (bool, error) {
	if tx == nil || !ids(principal.OrganizationID, principal.PrincipalID, projectID) {
		return false, nil
	}
	if principal.Role == "owner" || principal.Role == "admin" {
		return true, nil
	}
	if principal.Role != "member" {
		return false, nil
	}
	var ok bool
	err := tx.QueryRow(ctx, `SELECT true FROM workflow_project_admins
		WHERE organization_id=$1 AND project_id=$2 AND principal_id=$3 FOR SHARE`,
		principal.OrganizationID, projectID, principal.PrincipalID).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// CanAdministerProject reports whether caller administers projectID by the
// rule requireProjectAdmin enforces on project connection changes. It only
// informs the UI; every mutation rechecks inside its own transaction.
func (s *Store) CanAdministerProject(ctx context.Context, caller identity.Caller, projectID string) (bool, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, projectID) {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	return s.authz.CanAdministerProject(ctx, tx, caller, projectID)
}

// CanLaunch is the role rule LaunchRun enforces.
func CanLaunch(caller identity.Caller) bool {
	return caller.Role == "owner" || caller.Role == "admin" || caller.Role == "member"
}
