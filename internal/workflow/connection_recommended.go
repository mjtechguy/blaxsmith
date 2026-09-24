package workflow

import (
	"context"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

var recommendedModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)

// SetRecommendedModelsAs replaces a connection's pinned recommended models,
// in order. Whoever manages the connection may pin: organization owners and
// admins for organization connections, project admins for project ones, and
// the owner of a personal one.
func (s *Store) SetRecommendedModelsAs(ctx context.Context, caller identity.Caller, connectionID string, models []string) ([]string, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) || connectionID == "" || len(connectionID) > 64 || len(models) > 50 {
		return nil, ErrInvalid
	}
	seen := map[string]bool{}
	for _, m := range models {
		if !recommendedModelID.MatchString(m) || seen[m] {
			return nil, ErrInvalid
		}
		seen[m] = true
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return nil, err
	}
	r, err := lockConnection(ctx, tx, caller.OrganizationID, connectionID, true)
	if err != nil {
		return nil, err
	}
	switch r.OwnerKind {
	case "organization":
		if caller.Role != "owner" && caller.Role != "admin" {
			return nil, ErrConnectionDenied
		}
	case "project":
		if err := s.requireProjectAdmin(ctx, tx, caller, r.OwnerID); err != nil {
			return nil, err
		}
	default:
		if r.OwnerID != caller.PrincipalID {
			return nil, ErrNotFound
		}
	}
	if r.State != "active" {
		return nil, ErrInvalid
	}
	if _, err := tx.Exec(ctx, `DELETE FROM access_connection_recommended_models WHERE organization_id=$1 AND connection_id=$2`,
		caller.OrganizationID, connectionID); err != nil {
		return nil, err
	}
	for i, m := range models {
		if _, err := tx.Exec(ctx, `INSERT INTO access_connection_recommended_models
			(organization_id,connection_id,model_id,position,pinned_by) VALUES ($1,$2,$3,$4,$5)`,
			caller.OrganizationID, connectionID, m, i, caller.PrincipalID); err != nil {
			return nil, err
		}
	}
	return models, commitAudited(ctx, tx, caller, "access.connection.recommended_set", connectionID)
}

// recommendedModels is a connection's pinned models in pin order.
func (s *Store) recommendedModels(ctx context.Context, orgID, connectionID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT model_id FROM access_connection_recommended_models
		WHERE organization_id=$1 AND connection_id=$2 ORDER BY position`, orgID, connectionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}
