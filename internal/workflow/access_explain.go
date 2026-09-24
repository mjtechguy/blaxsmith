package workflow

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// AccessStep is one link of "where this comes from", closest to the
// principal first: a grant, then the resource it names, then (recipes) the
// version a new run would freeze.
type AccessStep struct{ Kind, Label, ID string }

type AccessExplanation struct {
	Usable         bool
	Steps          []AccessStep
	Reason         string // Why not, when not usable.
	GrantID        string // The resource grant that authorized it, if any.
	PrincipalLabel string
}

var roleReach = map[string]string{"member": "members and above", "admin": "admins and owners", "owner": "owners only"}

// ExplainAccessAs answers "may principalID (empty: the caller) use this
// connection or recipe in projectID, and through what?". Organization
// resources are decided by access.MatchingGrant, the same query CanUse runs,
// so the answer cannot drift from enforcement. Only organization owners and
// admins may ask about someone else; a member asking about a connection they
// cannot see learns only that it is not usable.
func (s *Store) ExplainAccessAs(ctx context.Context, caller identity.Caller, projectID, kind, resourceID, principalID string) (AccessExplanation, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, projectID) || resourceID == "" || len(resourceID) > 64 ||
		(kind != access.ResourceConnection && kind != access.ResourceRecipe) {
		return AccessExplanation{}, ErrInvalid
	}
	admin := caller.Role == "owner" || caller.Role == "admin"
	tx, err := s.pool.Begin(ctx) // Not read-only: MatchingGrant locks grant rows FOR SHARE.
	if err != nil {
		return AccessExplanation{}, err
	}
	defer tx.Rollback(ctx)
	if err := projectExists(ctx, tx, caller.OrganizationID, projectID); err != nil {
		return AccessExplanation{}, err
	}
	if kind == access.ResourceRecipe && !ids(resourceID) {
		return AccessExplanation{}, ErrInvalid
	}
	target := caller
	self := principalID == "" || principalID == caller.PrincipalID
	if !self {
		if !admin {
			return AccessExplanation{}, ErrConnectionDenied
		}
		if !ids(principalID) {
			return AccessExplanation{}, ErrInvalid
		}
		target = identity.Caller{OrganizationID: caller.OrganizationID, PrincipalID: principalID}
		err := tx.QueryRow(ctx, `SELECT m.role FROM identity_memberships m WHERE m.organization_id=$1 AND m.principal_id=$2`,
			caller.OrganizationID, principalID).Scan(&target.Role)
		if errors.Is(err, pgx.ErrNoRows) {
			return AccessExplanation{}, ErrNotFound
		}
		if err != nil {
			return AccessExplanation{}, err
		}
	}
	out := AccessExplanation{}
	if err := tx.QueryRow(ctx, `SELECT username FROM identity_principals WHERE id=$1`, target.PrincipalID).Scan(&out.PrincipalLabel); err != nil {
		return AccessExplanation{}, err
	}
	var projectName string
	if err := tx.QueryRow(ctx, `SELECT name FROM workflow_projects WHERE organization_id=$1 AND id=$2`,
		caller.OrganizationID, projectID).Scan(&projectName); err != nil {
		return AccessExplanation{}, err
	}
	var resource AccessStep
	var ownerKind, ownerID, state string
	var version int32
	var versionStep AccessStep
	if kind == access.ResourceConnection {
		var label, provider, account string
		err = tx.QueryRow(ctx, `SELECT c.owner_kind,c.owner_id,c.state,COALESCE(c.label,''),p.provider_kind,c.external_account_id
			FROM access_connections c JOIN access_provider_registrations p
				ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
			WHERE c.organization_id=$1 AND c.id=$2`, caller.OrganizationID, resourceID).
			Scan(&ownerKind, &ownerID, &state, &label, &provider, &account)
		if label == "" {
			label = provider
		}
		resource = AccessStep{Kind: map[string]string{"organization": "organization_connection", "project": "project_connection",
			"user": "personal_connection"}[ownerKind], Label: label, ID: resourceID}
	} else {
		var name, currentID string
		err = tx.QueryRow(ctx, `SELECT CASE WHEN r.project_id IS NULL THEN 'organization' ELSE 'project' END,
			COALESCE(r.project_id::text,''),r.name,COALESCE(r.current_version_id::text,''),COALESCE(v.version,0)
			FROM workflow_recipes r LEFT JOIN workflow_recipe_versions v ON v.organization_id=r.organization_id AND v.id=r.current_version_id
			WHERE r.organization_id=$1 AND r.id=$2::uuid`, caller.OrganizationID, resourceID).
			Scan(&ownerKind, &ownerID, &name, &currentID, &version)
		state = "active"
		resource = AccessStep{Kind: ownerKind + "_recipe", Label: name, ID: resourceID}
		versionStep = AccessStep{Kind: "recipe_version", Label: fmt.Sprintf("v%d (current)", version), ID: currentID}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return AccessExplanation{}, ErrNotFound
	}
	if err != nil {
		return AccessExplanation{}, err
	}
	if kind == access.ResourceConnection && !admin && self {
		if _, err := s.visibleConnection(ctx, caller, resourceID); errors.Is(err, ErrNotFound) {
			return AccessExplanation{PrincipalLabel: out.PrincipalLabel, Reason: "No grant makes it usable in this project; ask an organization admin."}, nil
		} else if err != nil {
			return AccessExplanation{}, err
		}
	}
	switch {
	case state != "active":
		out.Reason = "The connection is " + state + "."
	case ownerKind == "user":
		out.Usable = ownerID == target.PrincipalID
		out.Steps = []AccessStep{resource}
		if !out.Usable {
			out.Reason, out.Steps = "Personal connections serve only runs their owner launches.", nil
		}
	case ownerKind == "project":
		out.Usable = ownerID == projectID
		out.Steps = []AccessStep{resource}
		if !out.Usable {
			out.Reason, out.Steps = "It belongs to another project.", nil
		}
	default:
		match, ok, err := access.MatchingGrant(ctx, tx, target, projectID, kind, resourceID)
		if err != nil {
			return AccessExplanation{}, err
		}
		if !ok {
			out.Reason = "No grant for this project, user, or role; ask an organization admin."
			if target.Role == "viewer" {
				out.Reason = "Viewers never use organization " + kind + "s."
			}
			break
		}
		grant := AccessStep{Kind: match.Via + "_grant", ID: match.GrantID}
		switch match.Via {
		case "project":
			grant.Label = projectName
		case "user":
			grant.Label = out.PrincipalLabel
		default:
			grant.Label = roleReach[match.Role]
		}
		out.Usable, out.GrantID, out.Steps = true, match.GrantID, []AccessStep{grant, resource}
	}
	if out.Usable && version > 0 {
		out.Steps = append(out.Steps, versionStep)
	}
	return out, tx.Commit(ctx)
}
