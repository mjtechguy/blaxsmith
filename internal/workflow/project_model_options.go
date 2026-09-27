package workflow

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

type ProjectModelOption struct {
	ConnectionID, Label, Provider, OwnerKind, AuthMethod string
	Models                                               []ConnectionModel
	CheckedAt                                            *time.Time
	CatalogError                                         string
}

// Read-only model metadata follows invocation grants, not connection-management
// permissions. Never returns another user's personal connection or secrets.
func (s *Store) ProjectModelOptions(ctx context.Context, caller identity.Caller, project, harness string) ([]ProjectModelOption, error) {
	if !ids(caller.OrganizationID, caller.PrincipalID, project) || (harness != "" && !knownHarness(harness)) {
		return nil, ErrInvalid
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if _, err := s.GetProject(ctx, caller.OrganizationID, project); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT c.id,COALESCE(c.label,''),p.provider_kind,c.owner_kind,c.auth_method,g.resource `+modelGrantCandidates+` ORDER BY c.id,g.resource LIMIT 2001`, caller.OrganizationID, project, caller.PrincipalID)
	if err != nil {
		return nil, err
	}
	type granted struct{ id, label, provider, owner, auth, resource string }
	grants, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (granted, error) {
		var v granted
		err := row.Scan(&v.id, &v.label, &v.provider, &v.owner, &v.auth, &v.resource)
		return v, err
	})
	if err != nil {
		return nil, err
	}
	// Refuse an incomplete list rather than silently hiding granted accounts.
	if len(grants) > 2000 {
		return nil, ErrInvalid
	}
	connections := 0
	for i, g := range grants {
		if i == 0 || grants[i-1].id != g.id {
			connections++
		}
	}
	if connections > 200 {
		return nil, ErrInvalid
	}
	out := []ProjectModelOption{}
	for i := 0; i < len(grants); {
		g := grants[i]
		end := i + 1
		for end < len(grants) && grants[end].id == g.id {
			end++
		}
		option := ProjectModelOption{ConnectionID: g.id, Label: g.label, Provider: g.provider, OwnerKind: g.owner, AuthMethod: g.auth}
		models, checked, catalogErr, err := s.connectionModels(ctx, caller.OrganizationID, g.id, connectionRow{Provider: g.provider, AuthMethod: g.auth}, harness)
		if err != nil {
			return nil, err
		}
		option.CheckedAt, option.CatalogError = checked, catalogErr
		for _, grant := range grants[i:end] {
			model, ok := strings.CutPrefix(grant.resource, g.provider+"/")
			if !ok || model == "" {
				continue
			}
			supported := HarnessesFor(g.provider, model)
			if harness != "" && !slices.Contains(supported, harness) {
				continue
			}
			at := slices.IndexFunc(models, func(m ConnectionModel) bool { return m.ID == model })
			if at >= 0 {
				option.Models = append(option.Models, models[at])
			} else {
				// A valid grant may predate a catalog refresh. Keep the exact ID with
				// explicit missing metadata; approved-runtime preflight remains required.
				option.Models = append(option.Models, ConnectionModel{ID: model, DisplayName: model, Harnesses: supported, ModelMeta: access.ResolveModel(g.provider, model, access.ModelMeta{}, recipe.Efforts[harness])})
			}
		}
		if len(option.Models) > 0 {
			out = append(out, option)
		}
		i = end
	}
	return out, nil
}
