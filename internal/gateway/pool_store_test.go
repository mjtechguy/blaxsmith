package gateway

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func insertConnection(t *testing.T, pool *pgxpool.Pool, org, id, provider, ownerKind, owner, authMethod string) {
	t.Helper()
	ctx := tenant.System(t.Context())
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,$2,$3,'https://example.invalid',ARRAY['native_raw'],'active')`, org, "reg-"+id, provider); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,
		external_account_id,auth_method,state) VALUES ($1,$2,$3,$4,$5,'account',$6,'active')`,
		org, id, ownerKind, owner, "reg-"+id, authMethod); err != nil {
		t.Fatal(err)
	}
}

func insertRoute(t *testing.T, pool *pgxpool.Pool, f fixture, name, connection, kind, region string, priority int) (string, error) {
	t.Helper()
	var id string
	err := pool.QueryRow(tenant.System(t.Context()), `INSERT INTO gateway_routes (organization_id,name,connection_id,kind,region,priority,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`, f.org, name, connection, kind, region, priority, f.principal).Scan(&id)
	return id, err
}

// TestPoolsPlanPacingAndPolicyPostgres covers G2 end to end in the database:
// the pool/route policy triggers (no personal subscription can ever be a
// route or pool member), the plan a request gets, and the dispatcher's
// back-pressure: a saturated pool reports the stage queued with its reset
// time, and dispatch skips it until then.
func TestPoolsPlanPacingAndPolicyPostgres(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := tenant.System(t.Context())
	insertConnection(t, pool, f.org, "key-a", "anthropic", "organization", f.org, "api_key")
	insertConnection(t, pool, f.org, "key-b", "anthropic", "organization", f.org, "api_key")
	insertConnection(t, pool, f.org, "aws", "aws_bedrock", "organization", f.org, AWSSigV4AuthMethod)
	insertConnection(t, pool, f.org, "alice-claude", "anthropic", "user", f.principal, "claude_setup_token")
	insertConnection(t, pool, f.org, "alice-key", "anthropic", "user", f.principal, "api_key")
	insertConnection(t, pool, f.org, "org-codex", "openai", "organization", f.org, "codex_chatgpt")

	// Policy: personal subscriptions and user-owned keys are never routes.
	for _, connection := range []string{"alice-claude", "alice-key", "org-codex"} {
		if _, err := insertRoute(t, pool, f, "bad-"+connection, connection, KindAnthropic, "", 1); err == nil ||
			!strings.Contains(err.Error(), "organization-owned API-key or cloud connections only") {
			t.Fatalf("route over %s: %v", connection, err)
		}
	}
	if _, err := insertRoute(t, pool, f, "aws-as-anthropic", "aws", KindAnthropic, "", 1); err == nil {
		t.Fatal("cloud credential accepted as an Anthropic key route")
	}
	a, err := insertRoute(t, pool, f, "a", "key-a", KindAnthropic, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	b, err := insertRoute(t, pool, f, "b", "key-b", KindAnthropic, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	bedrock, err := insertRoute(t, pool, f, "bedrock", "aws", KindBedrock, "us-east-1", 3)
	if err != nil {
		t.Fatal(err)
	}
	var poolID, openaiPool string
	if err := pool.QueryRow(ctx, `INSERT INTO gateway_pools (organization_id,name,family,created_by) VALUES ($1,'Claude production','anthropic',$2)
		RETURNING id`, f.org, f.principal).Scan(&poolID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO gateway_pools (organization_id,name,family,created_by) VALUES ($1,'GPT','openai',$2)
		RETURNING id`, f.org, f.principal).Scan(&openaiPool); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{a, b, bedrock} {
		if _, err := pool.Exec(ctx, `INSERT INTO gateway_pool_routes (organization_id,pool_id,route_id) VALUES ($1,$2,$3)`, f.org, poolID, route); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_pool_routes (organization_id,pool_id,route_id) VALUES ($1,$2,$3)`, f.org, openaiPool, a); err == nil {
		t.Fatal("Anthropic route added to an OpenAI pool")
	}

	grant := Grant{OrganizationID: f.org, ProjectID: f.project, RunID: f.run, AttemptID: f.attempt, TaskID: f.task,
		Provider: "anthropic", Model: "claude-opus-5-5", ConnectionID: "key-a", AuthMethod: "api_key"}
	plan := func(g Grant) Plan {
		t.Helper()
		p, err := LoadPlan(ctx, pool, g)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	single := func(p Plan, id string) bool { return p.PoolID == "" && len(p.Routes) == 1 && p.Routes[0].ID == id }
	// Pools off (the default): one route per connection, as in G1.
	if p := plan(grant); !single(p, "key-a") {
		t.Fatalf("plan with pools off: %+v", p)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_org_settings (organization_id,enabled,pools_enabled,pacing_enabled)
		VALUES ($1,true,true,true)`, f.org); err != nil {
		t.Fatal(err)
	}
	// Pools on, but the pool is not granted to the project.
	if p := plan(grant); !single(p, "key-a") {
		t.Fatalf("ungranted pool used: %+v", p)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_resource_grants (organization_id,resource_kind,resource_id,grantee_project_id,granted_by)
		VALUES ($1,'gateway_pool',$2,$3,$4)`, f.org, poolID, f.project, f.principal); err != nil {
		t.Fatal(err)
	}
	p := plan(grant)
	if p.PoolID != poolID || p.PoolName != "Claude production" || len(p.Routes) != 3 || p.Routes[0].ID != a ||
		p.Routes[2].Kind != KindBedrock || p.Routes[2].AuthMethod != AWSSigV4AuthMethod {
		t.Fatalf("pool plan: %+v", p)
	}
	// A personal subscription is always its own single route, never pooled.
	personal := grant
	personal.ConnectionID, personal.AuthMethod = "alice-claude", "claude_setup_token"
	if p := plan(personal); !single(p, "alice-claude") || p.Routes[0].Kind != KindPersonal {
		t.Fatalf("personal plan: %+v", p)
	}
	// Disabled routes drop out of the plan.
	if _, err := pool.Exec(ctx, `UPDATE gateway_routes SET state='disabled' WHERE organization_id=$1 AND id=$2`, f.org, bedrock); err != nil {
		t.Fatal(err)
	}
	if p := plan(grant); len(p.Routes) != 2 {
		t.Fatalf("disabled route planned: %+v", p.Routes)
	}

	// Pacing: headroom while routes are healthy. (Dispatch skipping a paced
	// stage until its retry time is covered in workflow's stage flow test.)
	task := f.task
	request := PaceRequest{OrganizationID: f.org, ProjectID: f.project, RunID: f.run, TaskID: task, Family: "anthropic", ConnectionID: "key-a"}
	if err := Pace(ctx, pool, request); err != nil {
		t.Fatalf("healthy pool paced: %v", err)
	}
	// Saturate: route a has no requests left until its reset; route b is
	// cooling down after a 429. The G2 acceptance: the stage is queued with a
	// visible reason and the earliest reset time.
	states := NewStates()
	resetA := time.Now().Add(42 * time.Second).UTC().Truncate(time.Second)
	states.Begin(f.org, Route{ID: a}, f.attempt)(200, headers(map[string]string{
		"Anthropic-Ratelimit-Requests-Limit": "50", "Anthropic-Ratelimit-Requests-Remaining": "0",
		"Anthropic-Ratelimit-Requests-Reset": resetA.Format(time.RFC3339)}), 0)
	states.Begin(f.org, Route{ID: b}, f.attempt)(429, headers(map[string]string{"Retry-After": "90"}), 0)
	if err := states.Flush(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var paced *PacedError
	if err := Pace(ctx, pool, request); !errors.As(err, &paced) || !errors.Is(err, ErrPaced) {
		t.Fatalf("saturated pool not paced: %v", err)
	}
	if paced.Reason != "Waiting for Claude production headroom" || !paced.ResetAt.Equal(resetA) || paced.PoolID != poolID {
		t.Fatalf("paced: %+v (want reset %v)", paced, resetA)
	}
	var reason string
	var resets, retry time.Time
	if err := pool.QueryRow(ctx, `SELECT reason,resets_at,retry_at FROM gateway_paced_tasks WHERE organization_id=$1 AND task_id=$2`,
		f.org, task).Scan(&reason, &resets, &retry); err != nil || !resets.Equal(resetA) || retry.After(resetA.Add(time.Second)) {
		t.Fatalf("paced row: %q %v %v %v", reason, resets, retry, err)
	}
	// Headroom is back once the pacing switch is off: the row is cleared.
	if _, err := pool.Exec(ctx, `UPDATE gateway_org_settings SET pacing_enabled=false WHERE organization_id=$1`, f.org); err != nil {
		t.Fatal(err)
	}
	if err := Pace(ctx, pool, request); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway_paced_tasks WHERE organization_id=$1`, f.org).Scan(&left); err != nil || left != 0 {
		t.Fatalf("paced rows left: %d %v", left, err)
	}
	// Persisted state carries the metrics and cooldown the UI shows.
	var breaker string
	var cooldown *time.Time
	if err := pool.QueryRow(ctx, `SELECT breaker,cooldown_until FROM gateway_route_state WHERE organization_id=$1 AND route_id=$2`,
		f.org, b).Scan(&breaker, &cooldown); err != nil || breaker != BreakerClosed || cooldown == nil || !cooldown.After(time.Now().Add(60*time.Second)) {
		t.Fatalf("route b state: %s %v %v", breaker, cooldown, err)
	}
}

func headers(values map[string]string) map[string][]string {
	h := map[string][]string{}
	for k, v := range values {
		h[k] = []string{v}
	}
	return h
}

func TestSubscriptionLimitsAndRetentionPostgres(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := tenant.System(t.Context())
	g := Grant{OrganizationID: f.org, ConnectionID: "alice-codex", OwnerID: f.principal}
	reset := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	for _, used := range []float64{10, 64} {
		if err := RecordSubscriptionLimits(ctx, pool, g, []Window{{Name: "primary", UsedPct: used, WindowMinutes: 300, ResetsAt: reset}}); err != nil {
			t.Fatal(err)
		}
	}
	var used float64
	var minutes int
	var at time.Time
	if err := pool.QueryRow(ctx, `SELECT used_pct,window_minutes,resets_at FROM gateway_subscription_limits WHERE organization_id=$1`,
		f.org).Scan(&used, &minutes, &at); err != nil || used != 64 || minutes != 300 || !at.Equal(reset) {
		t.Fatalf("limits: %v %d %v %v", used, minutes, at, err)
	}
	if err := RecordSubscriptionLimits(ctx, pool, Grant{OrganizationID: f.org, ConnectionID: "x"}, []Window{{Name: "primary"}}); err == nil {
		t.Fatal("limits recorded without an owner")
	}
	// Retention follows the organization's gateway setting.
	grant := Grant{OrganizationID: f.org, ProjectID: f.project, RunID: f.run, AttemptID: f.attempt, TaskID: f.task, StageKey: "s", Harness: "codex"}
	for _, age := range []time.Duration{0, 10 * 24 * time.Hour, 40 * 24 * time.Hour} {
		if err := Record(ctx, pool, Event{Grant: grant, RouteID: "r", RouteKind: "openai", API: APIOther, Status: "ok",
			HTTPStatus: 200, StartedAt: time.Now().Add(-age)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_org_settings (organization_id,event_retention_days) VALUES ($1,7)`, f.org); err != nil {
		t.Fatal(err)
	}
	if err := PruneEvents(ctx, pool, 90*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway_usage_events WHERE organization_id=$1`, f.org).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("7-day retention kept %d events: %v", kept, err)
	}
}
