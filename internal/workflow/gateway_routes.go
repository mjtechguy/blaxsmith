package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Admin → Routes & pools (docs/model-gateway-plan.md §4, §5, §9.1) and the
// owner's own subscription meters (§6, §9.2).
//
// Policy: routes are organization-owned API-key or cloud connections only.
// A personal subscription (Codex sign-in, Claude setup-token) is never a
// route, never pooled, and never failed over to or from; the database
// refuses it too (gateway_route_connection_check).

type GatewayRouteMetric struct {
	Name             string
	Limit, Remaining int64
	ResetAt          *time.Time
}

type GatewayRoute struct {
	ID, Name, Kind, ConnectionID, ConnectionLabel string
	Region, CloudProject                          string
	AzureResource, APIVersion                     string // azure_openai
	ModelMap                                      map[string]string
	Weight, Priority, Cap, RequestsPerMinute      int
	TokensPerMinute                               int64
	State                                         string
	PoolIDs                                       []string
	// Live state from gateway_route_state.
	Breaker                string
	CooldownUntil, Last429 *time.Time
	StateUpdatedAt         *time.Time
	Inflight               int
	Metrics                []GatewayRouteMetric
	Requests15m, Errors15m int
}

type GatewayPool struct {
	ID, Name, Family, Strategy string
	Cap                        int
	Affinity                   bool
	State                      string
	RouteIDs, ProjectIDs       []string
}

type PacedStage struct {
	TaskID, RunID, ProjectID, Stage, PoolID, Reason string
	ResetsAt                                        *time.Time
	Since                                           time.Time
}

type GatewayOption struct{ ID, Label, Detail string }

type GatewayRoutesView struct {
	Routes                      []GatewayRoute
	Pools                       []GatewayPool
	Paced                       []PacedStage
	Connections, Projects       []GatewayOption
	PoolsEnabled, PacingEnabled bool
}

// CloudCredential is a validated Bedrock or Vertex credential for a new
// route's connection. The caller parses it (internal/gateway) first.
type CloudCredential struct {
	Secret          []byte
	ExternalAccount string // AWS access key id or service-account email; not secret.
}

var (
	routeName    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._-]{0,63}$`)
	routeModelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,199}$`)
	routeRegion  = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	cloudProject = regexp.MustCompile(`^[a-z][a-z0-9-]{4,62}$`)
	azureName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	apiVersion   = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}(-preview)?$`)
)

// routeProvider is the connection provider an API-key route kind serves.
var routeProvider = map[string]string{"anthropic": "anthropic", "openai": "openai", "opencode_zen": "opencode", "opencode_go": "opencode-go"}

func validRouteState(state string) bool {
	return state == "enabled" || state == "draining" || state == "disabled"
}

func routeError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23514", "23503", "22P02":
			return ErrInvalid
		case "23505":
			return ErrConflict
		}
	}
	return err
}

// GatewayRoutesAs lists routes with their live state, pools, paced stages,
// and the connections and projects the forms offer (owners and admins).
func (s *Store) GatewayRoutesAs(ctx context.Context, caller identity.Caller) (GatewayRoutesView, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GatewayRoutesView{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	settings, err := s.readGatewaySettings(ctx, tx, org, false)
	if err != nil {
		return GatewayRoutesView{}, err
	}
	view := GatewayRoutesView{Routes: []GatewayRoute{}, Pools: []GatewayPool{}, Paced: []PacedStage{},
		Connections: []GatewayOption{}, Projects: []GatewayOption{},
		PoolsEnabled: settings.Settings.PoolsEnabled, PacingEnabled: settings.Settings.PacingEnabled}
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.name,r.kind,r.connection_id,COALESCE(c.label,''),r.region,r.cloud_project,
		r.azure_resource,r.api_version,
		r.model_map,r.weight,r.priority,r.concurrency_cap,r.requests_per_minute,r.tokens_per_minute,r.state,
		ARRAY(SELECT pr.pool_id::text FROM gateway_pool_routes pr WHERE pr.organization_id=r.organization_id AND pr.route_id=r.id ORDER BY 1),
		COALESCE(st.breaker,'closed'),st.cooldown_until,COALESCE((SELECT sum(i.inflight) FROM gateway_route_inflight i WHERE i.organization_id=r.organization_id AND i.slot=r.id::text AND i.heartbeat_at>clock_timestamp()-interval '30 seconds'),st.inflight,0),COALESCE(st.metrics,'{}'),
		COALESCE(st.requests_15m,0),COALESCE(st.errors_15m,0),st.last_429_at,st.updated_at
		FROM gateway_routes r
		LEFT JOIN access_connections c ON c.organization_id=r.organization_id::text AND c.id=r.connection_id
		LEFT JOIN gateway_route_state st ON st.organization_id=r.organization_id AND st.route_id=r.id::text
		WHERE r.organization_id=$1 ORDER BY r.priority,r.name`, org)
	if err != nil {
		return GatewayRoutesView{}, err
	}
	for rows.Next() {
		var r GatewayRoute
		var mapping, metrics []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.ConnectionID, &r.ConnectionLabel, &r.Region, &r.CloudProject,
			&r.AzureResource, &r.APIVersion,
			&mapping, &r.Weight, &r.Priority, &r.Cap, &r.RequestsPerMinute, &r.TokensPerMinute, &r.State, &r.PoolIDs,
			&r.Breaker, &r.CooldownUntil, &r.Inflight, &metrics, &r.Requests15m, &r.Errors15m, &r.Last429, &r.StateUpdatedAt); err != nil {
			rows.Close()
			return GatewayRoutesView{}, err
		}
		_ = json.Unmarshal(mapping, &r.ModelMap)
		var parsed map[string]struct {
			Limit     int64     `json:"limit"`
			Remaining int64     `json:"remaining"`
			ResetAt   time.Time `json:"reset_at"`
		}
		_ = json.Unmarshal(metrics, &parsed)
		for name, m := range parsed {
			metric := GatewayRouteMetric{Name: name, Limit: m.Limit, Remaining: m.Remaining}
			if !m.ResetAt.IsZero() {
				at := m.ResetAt
				metric.ResetAt = &at
			}
			r.Metrics = append(r.Metrics, metric)
		}
		slices.SortFunc(r.Metrics, func(a, b GatewayRouteMetric) int { return compareStrings(a.Name, b.Name) })
		view.Routes = append(view.Routes, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return GatewayRoutesView{}, err
	}
	rows, err = tx.Query(ctx, `SELECT p.id::text,p.name,p.family,p.strategy,p.concurrency_cap,p.affinity,p.state,
		ARRAY(SELECT pr.route_id::text FROM gateway_pool_routes pr WHERE pr.organization_id=p.organization_id AND pr.pool_id=p.id ORDER BY 1),
		ARRAY(SELECT g.grantee_project_id::text FROM access_resource_grants g WHERE g.organization_id=p.organization_id
			AND g.resource_kind='gateway_pool' AND g.resource_id=p.id::text AND g.grantee_project_id IS NOT NULL
			AND g.revoked_at IS NULL ORDER BY 1)
		FROM gateway_pools p WHERE p.organization_id=$1 ORDER BY p.name`, org)
	if err != nil {
		return GatewayRoutesView{}, err
	}
	var pool GatewayPool
	if _, err := pgx.ForEachRow(rows, []any{&pool.ID, &pool.Name, &pool.Family, &pool.Strategy, &pool.Cap, &pool.Affinity,
		&pool.State, &pool.RouteIDs, &pool.ProjectIDs}, func() error {
		view.Pools = append(view.Pools, pool)
		return nil
	}); err != nil {
		return GatewayRoutesView{}, err
	}
	rows, err = tx.Query(ctx, `SELECT gp.task_id::text,gp.run_id::text,r.project_id::text,t.task_key,gp.pool_id::text,gp.reason,
		gp.resets_at,gp.since
		FROM gateway_paced_tasks gp
		JOIN workflow_tasks t ON t.organization_id=gp.organization_id AND t.id=gp.task_id AND t.state='pending'
		JOIN workflow_runs r ON r.organization_id=gp.organization_id AND r.id=gp.run_id
		WHERE gp.organization_id=$1 ORDER BY gp.since LIMIT 200`, org)
	if err != nil {
		return GatewayRoutesView{}, err
	}
	var paced PacedStage
	if _, err := pgx.ForEachRow(rows, []any{&paced.TaskID, &paced.RunID, &paced.ProjectID, &paced.Stage, &paced.PoolID,
		&paced.Reason, &paced.ResetsAt, &paced.Since}, func() error {
		view.Paced = append(view.Paced, paced)
		return nil
	}); err != nil {
		return GatewayRoutesView{}, err
	}
	rows, err = tx.Query(ctx, `SELECT c.id,COALESCE(NULLIF(c.label,''),p.provider_kind||' key'),p.provider_kind
		FROM access_connections c JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1 AND c.owner_kind='organization' AND c.auth_method='api_key' AND c.state='active'
		AND p.provider_kind = ANY($2) ORDER BY 2`, org, []string{"anthropic", "openai", "opencode", "opencode-go"})
	if err != nil {
		return GatewayRoutesView{}, err
	}
	var option GatewayOption
	if _, err := pgx.ForEachRow(rows, []any{&option.ID, &option.Label, &option.Detail}, func() error {
		view.Connections = append(view.Connections, option)
		return nil
	}); err != nil {
		return GatewayRoutesView{}, err
	}
	rows, err = tx.Query(ctx, `SELECT id::text,name,'' FROM workflow_projects WHERE organization_id=$1 ORDER BY name LIMIT 1000`, org)
	if err != nil {
		return GatewayRoutesView{}, err
	}
	if _, err := pgx.ForEachRow(rows, []any{&option.ID, &option.Label, &option.Detail}, func() error {
		view.Projects = append(view.Projects, option)
		return nil
	}); err != nil {
		return GatewayRoutesView{}, err
	}
	return view, tx.Commit(ctx)
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func validRoute(r GatewayRoute) bool {
	if !routeName.MatchString(r.Name) || r.Weight < 1 || r.Weight > 1000 || r.Priority < 0 || r.Priority > 1000 ||
		r.Cap < 0 || r.Cap > 10000 || r.RequestsPerMinute < 0 || r.RequestsPerMinute > 1_000_000 ||
		r.TokensPerMinute < 0 || r.TokensPerMinute > 1_000_000_000 || len(r.ModelMap) > 100 {
		return false
	}
	for ours, theirs := range r.ModelMap {
		if !routeModelID.MatchString(ours) || !routeModelID.MatchString(theirs) {
			return false
		}
	}
	regional := r.Kind == "bedrock" || r.Kind == "vertex"
	if regional != routeRegion.MatchString(r.Region) || cloudKind(r.Kind) && len(r.ModelMap) == 0 {
		return false
	}
	azure := r.Kind == "azure_openai"
	if azure != (azureName.MatchString(r.AzureResource) && apiVersion.MatchString(r.APIVersion)) ||
		!azure && (r.AzureResource != "" || r.APIVersion != "") {
		return false
	}
	return (r.Kind == "vertex") == cloudProject.MatchString(r.CloudProject)
}

// cloudKind routes carry their own credential as a route-only connection.
func cloudKind(kind string) bool {
	return kind == "bedrock" || kind == "vertex" || kind == "azure_openai"
}

// SaveGatewayRouteAs creates (empty ID) or updates a route. An API-key route
// names an organization API-key connection of the kind's provider. A cloud
// route (bedrock, vertex) is created with its credential, which becomes a
// new organization connection used only as a route; passing a credential
// on update rotates it. Kind and connection never change after creation.
func (s *Store) SaveGatewayRouteAs(ctx context.Context, caller identity.Caller, r GatewayRoute,
	credential *CloudCredential, secrets *access.SecretStore) (GatewayRoute, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if r.Weight == 0 {
		r.Weight = 1
	}
	if r.State == "" {
		r.State = "enabled"
	}
	if r.ModelMap == nil {
		r.ModelMap = map[string]string{}
	}
	if !validRouteState(r.State) || (r.ID != "" && !ids(r.ID)) {
		return GatewayRoute{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GatewayRoute{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	cloud := cloudKind(r.Kind)
	action := "gateway.route.updated"
	if r.ID != "" {
		var connection string
		if err := tx.QueryRow(ctx, `SELECT kind,connection_id,azure_resource FROM gateway_routes WHERE organization_id=$1 AND id=$2 FOR UPDATE`,
			org, r.ID).Scan(&r.Kind, &connection, &r.AzureResource); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return GatewayRoute{}, ErrNotFound
			}
			return GatewayRoute{}, err
		}
		r.ConnectionID = connection
		cloud = cloudKind(r.Kind)
	}
	if !validRoute(r) || (credential != nil && (!cloud || len(credential.Secret) == 0)) {
		return GatewayRoute{}, ErrInvalid
	}
	if r.ID == "" {
		action = "gateway.route.created"
		switch {
		case cloud:
			if credential == nil || secrets == nil {
				return GatewayRoute{}, ErrInvalid
			}
			provider, method, origin := "aws_bedrock", "aws_sigv4", "https://bedrock-runtime."+r.Region+".amazonaws.com"
			switch r.Kind {
			case "vertex":
				provider, method, origin = "gcp_vertex", "gcp_service_account", "https://aiplatform.googleapis.com"
			case "azure_openai":
				provider, method, origin = "azure_openai", "azure_api_key", "https://"+r.AzureResource+".openai.azure.com"
			}
			var providerID string
			if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
				VALUES ($1,gen_random_uuid()::text,$2,$3,ARRAY['brokered_gateway'],'active') RETURNING id`, org, provider, origin).Scan(&providerID); err != nil {
				return GatewayRoute{}, err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO access_connections
				(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,label)
				VALUES ($1,gen_random_uuid()::text,'organization',$1,$2,$3,$4,'active',$5) RETURNING id`,
				org, providerID, credential.ExternalAccount, method, r.Name).Scan(&r.ConnectionID); err != nil {
				return GatewayRoute{}, err
			}
			if _, err := secrets.RotateTx(ctx, tx, org, r.ConnectionID, 0, credential.Secret, nil); err != nil {
				return GatewayRoute{}, err
			}
			if err := audit(ctx, tx, caller, "access.connection.created", r.ConnectionID); err != nil {
				return GatewayRoute{}, err
			}
		default:
			provider, ok := routeProvider[r.Kind]
			if !ok {
				return GatewayRoute{}, ErrInvalid
			}
			var matches bool
			if err := tx.QueryRow(ctx, `SELECT p.provider_kind=$3 FROM access_connections c
				JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
				WHERE c.organization_id=$1 AND c.id=$2`, org, r.ConnectionID, provider).Scan(&matches); err != nil || !matches {
				return GatewayRoute{}, ErrInvalid
			}
		}
		mapping, _ := json.Marshal(r.ModelMap)
		if err := tx.QueryRow(ctx, `INSERT INTO gateway_routes (organization_id,name,connection_id,kind,region,cloud_project,model_map,
			weight,priority,concurrency_cap,requests_per_minute,tokens_per_minute,state,created_by,azure_resource,api_version)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id::text`, org, r.Name, r.ConnectionID, r.Kind,
			r.Region, r.CloudProject, mapping, r.Weight, r.Priority, r.Cap, r.RequestsPerMinute, r.TokensPerMinute, r.State,
			caller.PrincipalID, r.AzureResource, r.APIVersion).Scan(&r.ID); err != nil {
			return GatewayRoute{}, routeError(err)
		}
	} else {
		mapping, _ := json.Marshal(r.ModelMap)
		if _, err := tx.Exec(ctx, `UPDATE gateway_routes SET name=$3,region=$4,cloud_project=$5,model_map=$6,weight=$7,priority=$8,
			concurrency_cap=$9,requests_per_minute=$10,tokens_per_minute=$11,state=$12,api_version=$13,updated_at=clock_timestamp()
			WHERE organization_id=$1 AND id=$2`, org, r.ID, r.Name, r.Region, r.CloudProject, mapping, r.Weight, r.Priority,
			r.Cap, r.RequestsPerMinute, r.TokensPerMinute, r.State, r.APIVersion); err != nil {
			return GatewayRoute{}, routeError(err)
		}
		if credential != nil {
			var version *int64
			if err := tx.QueryRow(ctx, `SELECT active_secret_version FROM access_connections WHERE organization_id=$1 AND id=$2`,
				org, r.ConnectionID).Scan(&version); err != nil {
				return GatewayRoute{}, err
			}
			current := int64(0)
			if version != nil {
				current = *version
			}
			if _, err := secrets.RotateTx(ctx, tx, org, r.ConnectionID, current, credential.Secret, nil); err != nil {
				return GatewayRoute{}, err
			}
			if err := audit(ctx, tx, caller, "access.connection.rotated", r.ConnectionID); err != nil {
				return GatewayRoute{}, err
			}
		}
	}
	if err := audit(ctx, tx, caller, action, r.ID); err != nil {
		return GatewayRoute{}, err
	}
	return r, tx.Commit(ctx)
}

// SetGatewayRouteStateAs drains, disables or re-enables a route (§9.1).
func (s *Store) SetGatewayRouteStateAs(ctx context.Context, caller identity.Caller, routeID, state string) error {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(routeID) || !validRouteState(state) {
		return ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `UPDATE gateway_routes SET state=$3,updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2`,
		caller.OrganizationID, routeID, state)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return commitAudited(ctx, tx, caller, "gateway.route."+state, routeID)
}

// SaveGatewayPoolAs creates (empty ID) or updates a pool with its member
// routes and the projects it is granted to. Members must serve the pool's
// family; the database refuses anything else.
func (s *Store) SaveGatewayPoolAs(ctx context.Context, caller identity.Caller, p GatewayPool) (GatewayPool, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if p.Strategy == "" {
		p.Strategy = "priority_headroom"
	}
	if p.State == "" {
		p.State = "enabled"
	}
	// Empty, never NULL, so "= ANY" below revokes everything left out.
	p.RouteIDs = append([]string{}, p.RouteIDs...)
	p.ProjectIDs = append([]string{}, p.ProjectIDs...)
	if !routeName.MatchString(p.Name) || (p.ID != "" && !ids(p.ID)) || p.Cap < 0 || p.Cap > 100000 ||
		!slices.Contains([]string{"anthropic", "openai", "opencode", "opencode-go"}, p.Family) ||
		!slices.Contains([]string{"priority_headroom", "weighted", "fill_first"}, p.Strategy) ||
		(p.State != "enabled" && p.State != "disabled") || len(p.RouteIDs) > 50 || len(p.ProjectIDs) > 1000 ||
		!ids(p.RouteIDs...) || !ids(p.ProjectIDs...) {
		return GatewayPool{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GatewayPool{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	action := "gateway.pool.updated"
	if p.ID == "" {
		action = "gateway.pool.created"
		if err := tx.QueryRow(ctx, `INSERT INTO gateway_pools (organization_id,name,family,strategy,concurrency_cap,affinity,state,created_by)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id::text`, org, p.Name, p.Family, p.Strategy, p.Cap, p.Affinity, p.State,
			caller.PrincipalID).Scan(&p.ID); err != nil {
			return GatewayPool{}, routeError(err)
		}
	} else {
		tag, err := tx.Exec(ctx, `UPDATE gateway_pools SET name=$3,strategy=$4,concurrency_cap=$5,affinity=$6,state=$7,
			updated_at=clock_timestamp() WHERE organization_id=$1 AND id=$2 AND family=$8`,
			org, p.ID, p.Name, p.Strategy, p.Cap, p.Affinity, p.State, p.Family)
		if err != nil {
			return GatewayPool{}, routeError(err)
		}
		if tag.RowsAffected() == 0 {
			return GatewayPool{}, ErrNotFound
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM gateway_pool_routes WHERE organization_id=$1 AND pool_id=$2 AND NOT (route_id::text = ANY($3))`,
		org, p.ID, p.RouteIDs); err != nil {
		return GatewayPool{}, err
	}
	for _, route := range p.RouteIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO gateway_pool_routes (organization_id,pool_id,route_id) VALUES ($1,$2,$3)
			ON CONFLICT DO NOTHING`, org, p.ID, route); err != nil {
			return GatewayPool{}, routeError(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE access_resource_grants SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND resource_kind='gateway_pool' AND resource_id=$2 AND revoked_at IS NULL
		AND (grantee_project_id IS NULL OR NOT (grantee_project_id::text = ANY($3)))`, org, p.ID, p.ProjectIDs); err != nil {
		return GatewayPool{}, err
	}
	for _, project := range p.ProjectIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO access_resource_grants (organization_id,resource_kind,resource_id,grantee_project_id,granted_by)
			SELECT $1,'gateway_pool',$2,$3,$4 WHERE NOT EXISTS (SELECT 1 FROM access_resource_grants
				WHERE organization_id=$1 AND resource_kind='gateway_pool' AND resource_id=$2 AND grantee_project_id=$3 AND revoked_at IS NULL)`,
			org, p.ID, project, caller.PrincipalID); err != nil {
			return GatewayPool{}, routeError(err)
		}
	}
	if err := audit(ctx, tx, caller, action, p.ID); err != nil {
		return GatewayPool{}, err
	}
	return p, tx.Commit(ctx)
}

type SubscriptionWindow struct {
	Name          string
	UsedPct       float64
	WindowMinutes int
	ResetsAt      *time.Time
	ObservedAt    time.Time
}

type SubscriptionLimits struct {
	ConnectionID, Label, Provider, AuthMethod, State string
	Windows                                          []SubscriptionWindow
}

// MySubscriptionLimitsAs lists the caller's own personal subscription
// connections with the usage windows their providers reported on the
// caller's own runs. Nobody else's subscriptions are ever included; admins
// see only aggregate usage elsewhere, never these.
func (s *Store) MySubscriptionLimitsAs(ctx context.Context, caller identity.Caller) ([]SubscriptionLimits, bool, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID) {
		return nil, false, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, true); err != nil {
		return nil, false, err
	}
	settings, err := s.readGatewaySettings(ctx, tx, caller.OrganizationID, false)
	if err != nil {
		return nil, false, err
	}
	rows, err := tx.Query(ctx, `SELECT c.id,COALESCE(c.label,''),p.provider_kind,c.auth_method,c.state,
		COALESCE(l.window_name,''),COALESCE(l.used_pct,0),COALESCE(l.window_minutes,0),l.resets_at,COALESCE(l.observed_at,c.created_at)
		FROM access_connections c
		JOIN access_provider_registrations p ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		LEFT JOIN gateway_subscription_limits l ON l.organization_id::text=c.organization_id AND l.connection_id=c.id
			AND l.principal_id::text=c.owner_id
		WHERE c.organization_id=$1 AND c.owner_kind='user' AND c.owner_id=$2
		AND c.auth_method = ANY($3) AND c.state<>'revoked'
		ORDER BY c.created_at,c.id,l.window_name`, caller.OrganizationID, caller.PrincipalID,
		[]string{access.CodexSubscriptionAuth, access.ClaudeSetupTokenAuth})
	if err != nil {
		return nil, false, err
	}
	out := []SubscriptionLimits{}
	var id, label, provider, method, state, window string
	var used float64
	var minutes int
	var resets *time.Time
	var observed time.Time
	if _, err := pgx.ForEachRow(rows, []any{&id, &label, &provider, &method, &state, &window, &used, &minutes, &resets, &observed}, func() error {
		if len(out) == 0 || out[len(out)-1].ConnectionID != id {
			out = append(out, SubscriptionLimits{ConnectionID: id, Label: label, Provider: provider, AuthMethod: method, State: state,
				Windows: []SubscriptionWindow{}})
		}
		if window != "" {
			last := &out[len(out)-1]
			last.Windows = append(last.Windows, SubscriptionWindow{Name: window, UsedPct: used, WindowMinutes: minutes,
				ResetsAt: resets, ObservedAt: observed})
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	return out, settings.Settings.PersonalRoutesEnabled, tx.Commit(ctx)
}

// GatewayRouteTraffic is one pool route's traffic over a window.
type GatewayRouteTraffic struct {
	RouteID, RouteName, RouteKind                        string
	Requests, Errors, RateLimited, FailoversFrom, Tokens int64
	CostUSDMicros, AvgTTFTMS                             int64
}

// GatewayFailover is one request that failed on a route before its first
// byte and was retried on the next (both are usage events, §4).
type GatewayFailover struct {
	At                                                 time.Time
	RunID, ProjectID, Stage                            string
	FromRouteID, FromRouteName, ToRouteID, ToRouteName string
	HTTPStatus, FinalHTTPStatus                        int
	FinalStatus                                        string
}

type GatewayPoolDetail struct {
	Pool      GatewayPool
	Traffic   []GatewayRouteTraffic
	Failovers []GatewayFailover
	Hours     int
}

// GatewayPoolDetailAs is Admin → Routes & pools → pool: Traffic and
// Failovers tabs, built from the per-attempt usage events (owners, admins).
func (s *Store) GatewayPoolDetailAs(ctx context.Context, caller identity.Caller, poolID string, hours int) (GatewayPoolDetail, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !ids(poolID) || hours < 0 || hours > 168 {
		return GatewayPoolDetail{}, ErrInvalid
	}
	if hours == 0 {
		hours = 24
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GatewayPoolDetail{}, adminScopeError(err)
	}
	defer tx.Rollback(ctx)
	org := caller.OrganizationID
	d := GatewayPoolDetail{Hours: hours, Traffic: []GatewayRouteTraffic{}, Failovers: []GatewayFailover{}}
	p := &d.Pool
	err = tx.QueryRow(ctx, `SELECT p.id::text,p.name,p.family,p.strategy,p.concurrency_cap,p.affinity,p.state,
		ARRAY(SELECT pr.route_id::text FROM gateway_pool_routes pr WHERE pr.organization_id=p.organization_id AND pr.pool_id=p.id ORDER BY 1),
		ARRAY(SELECT g.grantee_project_id::text FROM access_resource_grants g WHERE g.organization_id=p.organization_id
			AND g.resource_kind='gateway_pool' AND g.resource_id=p.id::text AND g.grantee_project_id IS NOT NULL
			AND g.revoked_at IS NULL ORDER BY 1)
		FROM gateway_pools p WHERE p.organization_id=$1 AND p.id=$2`, org, poolID).Scan(&p.ID, &p.Name, &p.Family, &p.Strategy,
		&p.Cap, &p.Affinity, &p.State, &p.RouteIDs, &p.ProjectIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return GatewayPoolDetail{}, ErrNotFound
	}
	if err != nil {
		return GatewayPoolDetail{}, err
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour)
	rows, err := tx.Query(ctx, `SELECT r.id::text,r.name,r.kind,count(e.id),
		count(e.id) FILTER (WHERE e.status<>'ok'),count(e.id) FILTER (WHERE e.http_status=429),
		count(e.id) FILTER (WHERE e.status='error' AND EXISTS (SELECT 1 FROM gateway_usage_events n
			WHERE n.organization_id=e.organization_id AND n.attempt_id=e.attempt_id AND n.pool_id=e.pool_id
			AND n.retry_count=e.retry_count+1 AND n.started_at>=e.started_at AND n.started_at<e.started_at+interval '10 minutes')),
		COALESCE(sum(e.input_tokens+e.output_tokens+e.cache_read_tokens+e.cache_write_tokens),0),
		COALESCE(sum(e.cost_usd_micros),0),COALESCE(avg(e.ttft_ms),0)::bigint
		FROM gateway_pool_routes pr
		JOIN gateway_routes r ON r.organization_id=pr.organization_id AND r.id=pr.route_id
		LEFT JOIN gateway_usage_events e ON e.organization_id=r.organization_id AND e.pool_id=$2::text AND e.route_id=r.id::text
			AND e.started_at>=$3
		WHERE pr.organization_id=$1 AND pr.pool_id=$2::uuid
		GROUP BY r.id,r.name,r.kind,r.priority ORDER BY r.priority,r.name`, org, poolID, since)
	if err != nil {
		return GatewayPoolDetail{}, err
	}
	var t GatewayRouteTraffic
	if _, err := pgx.ForEachRow(rows, []any{&t.RouteID, &t.RouteName, &t.RouteKind, &t.Requests, &t.Errors, &t.RateLimited,
		&t.FailoversFrom, &t.Tokens, &t.CostUSDMicros, &t.AvgTTFTMS}, func() error {
		d.Traffic = append(d.Traffic, t)
		return nil
	}); err != nil {
		return GatewayPoolDetail{}, err
	}
	rows, err = tx.Query(ctx, `SELECT e.started_at,e.run_id::text,e.project_id::text,e.stage_key,e.route_id,COALESCE(fr.name,e.route_id),
		e.http_status,n.route_id,COALESCE(tr.name,n.route_id),n.status,n.http_status
		FROM gateway_usage_events e
		JOIN LATERAL (SELECT x.route_id,x.status,x.http_status FROM gateway_usage_events x
			WHERE x.organization_id=e.organization_id AND x.attempt_id=e.attempt_id AND x.pool_id=e.pool_id
			AND x.retry_count=e.retry_count+1 AND x.started_at>=e.started_at AND x.started_at<e.started_at+interval '10 minutes'
			ORDER BY x.started_at LIMIT 1) n ON true
		LEFT JOIN gateway_routes fr ON fr.organization_id=e.organization_id AND fr.id::text=e.route_id
		LEFT JOIN gateway_routes tr ON tr.organization_id=e.organization_id AND tr.id::text=n.route_id
		WHERE e.organization_id=$1 AND e.pool_id=$2 AND e.started_at>=$3 AND e.status='error'
		ORDER BY e.started_at DESC LIMIT 200`, org, poolID, since)
	if err != nil {
		return GatewayPoolDetail{}, err
	}
	var f GatewayFailover
	if _, err := pgx.ForEachRow(rows, []any{&f.At, &f.RunID, &f.ProjectID, &f.Stage, &f.FromRouteID, &f.FromRouteName,
		&f.HTTPStatus, &f.ToRouteID, &f.ToRouteName, &f.FinalStatus, &f.FinalHTTPStatus}, func() error {
		d.Failovers = append(d.Failovers, f)
		return nil
	}); err != nil {
		return GatewayPoolDetail{}, err
	}
	return d, tx.Commit(ctx)
}
