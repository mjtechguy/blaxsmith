package workflow

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// ConnectionModels lists the model IDs a model-provider connection offers.
// merge: lane C ListConnectionModels
type ConnectionModels interface {
	ListConnectionModels(ctx context.Context, orgID, connectionID string) ([]string, error)
}

// grantedConnectionModels is the stub until connections store their model
// lists: the exact models already granted to projects through the connection.
// merge: lane C ListConnectionModels
type grantedConnectionModels struct{ s *Store }

func (g grantedConnectionModels) ListConnectionModels(ctx context.Context, orgID, connectionID string) ([]string, error) {
	rows, err := g.s.pool.Query(ctx, `SELECT DISTINCT m.model FROM workflow_project_model_grants m
		JOIN access_grants a ON a.organization_id=m.organization_id::text AND a.id=m.grant_id
		WHERE m.organization_id=$1 AND a.connection_id=$2 AND m.revoked_at IS NULL ORDER BY m.model LIMIT 200`,
		orgID, connectionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// SetConnectionModels replaces the connection model-list source.
func (s *Store) SetConnectionModels(models ConnectionModels) { s.models = models }

type ModelConnection struct {
	ID, Provider, Account string
	Models                []string
}

// RecipeModelConnections lists active organization model connections and
// their models for the recipe editor. It returns no secret material.
func (s *Store) RecipeModelConnections(ctx context.Context, caller identity.Caller) ([]ModelConnection, error) {
	if !ids(caller.OrganizationID) {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT c.id,p.provider_kind,c.external_account_id
		FROM access_connections c JOIN access_provider_registrations p
			ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1::text AND c.state='active' AND p.provider_kind IN ('openai','anthropic')
		ORDER BY p.provider_kind,c.created_at,c.id LIMIT 100`, caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ModelConnection, error) {
		var c ModelConnection
		return c, row.Scan(&c.ID, &c.Provider, &c.Account)
	})
	if err != nil {
		return nil, err
	}
	var models ConnectionModels = grantedConnectionModels{s}
	if s.models != nil {
		models = s.models
	}
	for i := range out {
		if out[i].Models, err = models.ListConnectionModels(ctx, caller.OrganizationID, out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}
