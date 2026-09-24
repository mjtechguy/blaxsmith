package workflow

import (
	"context"
	"errors"

	"github.com/mjtechguy/blaxsmith/internal/identity"
)

type ModelConnection struct {
	ID, Provider, Account string
	Models                []string
}

// RecipeModelConnections lists active organization model connections and
// their stored model lists (lane C's ListConnectionModelsAs) for the recipe
// editor. Only recipe editors (organization owners/admins) see connections.
// It returns no secret material.
func (s *Store) RecipeModelConnections(ctx context.Context, caller identity.Caller) ([]ModelConnection, error) {
	if !ids(caller.OrganizationID) {
		return nil, ErrInvalid
	}
	if caller.Role != "owner" && caller.Role != "admin" {
		return nil, nil
	}
	connections, err := s.ListConnectionsAs(ctx, caller, ScopeOrganization, "")
	if errors.Is(err, ErrConnectionDenied) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []ModelConnection
	for _, c := range connections {
		if c.State != "active" || c.Kind == "git" || c.Kind == "oauth_app" {
			continue
		}
		models, _, _, err := s.ListConnectionModelsAs(ctx, caller, c.ID, "")
		if err != nil {
			return nil, err
		}
		mc := ModelConnection{ID: c.ID, Provider: c.Provider, Account: c.Account}
		for _, m := range models {
			mc.Models = append(mc.Models, m.ID)
		}
		out = append(out, mc)
	}
	return out, nil
}
