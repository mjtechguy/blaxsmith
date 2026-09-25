package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
)

// Route kinds (§4). personal_subscription is never stored as a route row
// and never pooled: it is the owner's own connection serving the owner's run.
const (
	KindAnthropic = "anthropic"
	KindBedrock   = "bedrock"
	KindVertex    = "vertex"
	KindOpenAI    = "openai"
	KindPersonal  = "personal_subscription"
)

// Pool strategies (§4).
const (
	StrategyPriorityHeadroom = "priority_headroom"
	StrategyWeighted         = "weighted"
	StrategyFillFirst        = "fill_first"
)

// Cloud connection auth methods: organization-owned credentials that only
// ever serve as pool routes, never granted to a project directly.
const (
	AWSSigV4AuthMethod          = "aws_sigv4"
	GCPServiceAccountAuthMethod = "gcp_service_account"
)

// Route is one upstream the gateway may send a request to.
type Route struct {
	ID, Name, Kind, ConnectionID, AuthMethod string
	Region, CloudProject                     string
	ModelMap                                 map[string]string
	Weight, Priority, Cap                    int
	RequestsPerMinute                        int
	TokensPerMinute                          int64
	State                                    string // enabled, draining, disabled
}

// Personal reports a personal subscription route (owner-only, never pooled).
func (r Route) Personal() bool { return r.Kind == KindPersonal }

// model maps our model id to the route's. Cloud routes need an explicit
// mapping (Bedrock and Vertex ids differ from Anthropic's); other kinds pass
// ids through unless a map restricts them.
func (r Route) model(ours string) (string, bool) {
	if mapped, ok := r.ModelMap[ours]; ok && mapped != "" {
		return mapped, true
	}
	if r.Kind == KindBedrock || r.Kind == KindVertex {
		return "", false
	}
	return ours, len(r.ModelMap) == 0
}

// serves reports whether the route's transport carries this endpoint.
func (r Route) serves(path string) bool {
	switch r.Kind {
	case KindBedrock, KindVertex:
		return path == "/v1/messages"
	case KindPersonal:
		if r.AuthMethod == access.CodexSubscriptionAuth {
			return path == "/v1/responses" || path == "/v1/responses/compact" || path == "/v1/models" ||
				len(path) > len("/v1/models/") && path[:len("/v1/models/")] == "/v1/models/"
		}
	}
	return true
}

// Plan is the set of routes one request may use, in pool order.
type Plan struct {
	PoolID, PoolName, Strategy string
	PoolCap                    int
	Affinity                   bool
	Routes                     []Route
}

// implicitRoute is the G1 one-connection route for the leased connection;
// its id is the connection id, as G1 usage events record it.
func implicitRoute(g Grant) Route {
	kind := RouteKind(g.Provider)
	if subscriptionAuth(g.AuthMethod) {
		kind = KindPersonal
	}
	return Route{ID: g.ConnectionID, Name: "connection", Kind: kind, ConnectionID: g.ConnectionID,
		AuthMethod: g.AuthMethod, Weight: 1, Priority: 100, State: "enabled"}
}

func subscriptionAuth(method string) bool {
	return method == access.CodexSubscriptionAuth || method == access.ClaudeSetupTokenAuth
}

// LoadPlan picks the routes for one authorized request (§4). A personal
// subscription is always its own single route. Otherwise, with the org's
// Pools & failover switch on, the pool that contains the leased connection's
// route, serves this family, and is granted to the run's project supplies
// the routes; without one, the leased connection is the only route (G1).
//
// ponytail: two indexed reads per request while pools are on, no cache.
func LoadPlan(ctx context.Context, db *pgxpool.Pool, g Grant) (Plan, error) {
	single := Plan{Routes: []Route{implicitRoute(g)}}
	if db == nil || subscriptionAuth(g.AuthMethod) {
		return single, nil
	}
	var plan Plan
	err := db.QueryRow(ctx, poolForConnection+` ORDER BY p.name LIMIT 1`,
		g.OrganizationID, g.Provider, g.ConnectionID, g.ProjectID).Scan(&plan.PoolID, &plan.PoolName, &plan.Strategy,
		&plan.PoolCap, &plan.Affinity)
	if errors.Is(err, pgx.ErrNoRows) {
		return single, nil
	}
	if err != nil {
		return Plan{}, fmt.Errorf("gateway pool lookup: %w", err)
	}
	routes, err := poolRoutes(ctx, db, g.OrganizationID, plan.PoolID, false)
	if err != nil {
		return Plan{}, err
	}
	plan.Routes = routes
	if len(plan.Routes) == 0 {
		return single, nil
	}
	return plan, nil
}

// poolForConnection finds the enabled pool, granted to the project, that
// contains the route for the leased connection. Only while the organization
// has Pools & failover on. $1 org, $2 family, $3 connection, $4 project.
const poolForConnection = `SELECT p.id::text,p.name,p.strategy,p.concurrency_cap,p.affinity
	FROM gateway_pools p
	JOIN gateway_org_settings s ON s.organization_id=p.organization_id AND s.enabled AND s.pools_enabled
	JOIN gateway_pool_routes pr ON pr.organization_id=p.organization_id AND pr.pool_id=p.id
	JOIN gateway_routes r ON r.organization_id=pr.organization_id AND r.id=pr.route_id
	WHERE p.organization_id=$1 AND p.family=$2 AND p.state='enabled' AND r.connection_id=$3
	AND EXISTS (SELECT 1 FROM access_resource_grants gr WHERE gr.organization_id=p.organization_id
		AND gr.resource_kind='gateway_pool' AND gr.resource_id=p.id::text AND gr.grantee_project_id=$4::uuid
		AND gr.revoked_at IS NULL)`

// poolRoutes lists a pool's routes over active organization connections.
// Disabled routes are left out unless all is set (the admin view).
func poolRoutes(ctx context.Context, db *pgxpool.Pool, orgID, poolID string, all bool) ([]Route, error) {
	rows, err := db.Query(ctx, `SELECT r.id::text,r.name,r.kind,r.connection_id,c.auth_method,r.region,r.cloud_project,
		r.model_map,r.weight,r.priority,r.concurrency_cap,r.requests_per_minute,r.tokens_per_minute,r.state
		FROM gateway_pool_routes pr
		JOIN gateway_routes r ON r.organization_id=pr.organization_id AND r.id=pr.route_id
		JOIN access_connections c ON c.organization_id=r.organization_id::text AND c.id=r.connection_id
			AND c.state='active' AND c.owner_kind='organization'
		WHERE pr.organization_id=$1 AND pr.pool_id=$2 AND ($3 OR r.state<>'disabled')
		ORDER BY r.priority,r.name`, orgID, poolID, all)
	if err != nil {
		return nil, fmt.Errorf("gateway pool routes: %w", err)
	}
	defer rows.Close()
	var out []Route
	for rows.Next() {
		var r Route
		var mapping []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.Kind, &r.ConnectionID, &r.AuthMethod, &r.Region, &r.CloudProject,
			&mapping, &r.Weight, &r.Priority, &r.Cap, &r.RequestsPerMinute, &r.TokensPerMinute, &r.State); err != nil {
			return nil, err
		}
		if subscriptionAuth(r.AuthMethod) { // unreachable past the route trigger; never pool one anyway.
			continue
		}
		_ = json.Unmarshal(mapping, &r.ModelMap)
		out = append(out, r)
	}
	return out, rows.Err()
}
