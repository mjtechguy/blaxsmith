package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestGatewayRoutesPoolsAndMyLimits(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "gw-routes")
	owner := reviewer(t, pool, org, "owner", "gw-routes-owner")
	member := reviewer(t, pool, org, "member", "gw-routes-member")
	other := reviewer(t, pool, org, "member", "gw-routes-other")
	project, err := store.CreateProjectAs(ctx, member, "gw-routes-project", "Routes project")
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := access.NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	connection := func(id, provider, ownerKind, ownerID, method string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ($1,$2,$3,'https://example.invalid',ARRAY['native_raw'],'active')`, org, "reg-"+id, provider); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,
			external_account_id,auth_method,state,label) VALUES ($1,$2,$3,$4,$5,'acct',$6,'active',$2)`,
			org, id, ownerKind, ownerID, "reg-"+id, method); err != nil {
			t.Fatal(err)
		}
	}
	connection("org-anthropic", "anthropic", "organization", org, "api_key")
	connection("org-openai", "openai", "organization", org, "api_key")
	connection("member-claude", "anthropic", "user", member.PrincipalID, access.ClaudeSetupTokenAuth)
	connection("other-codex", "openai", "user", other.PrincipalID, access.CodexSubscriptionAuth)

	route := GatewayRoute{Name: "anthropic-a", Kind: "anthropic", ConnectionID: "org-anthropic", Priority: 1}
	if _, err := store.SaveGatewayRouteAs(ctx, member, route, nil, secrets); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member created a route: %v", err)
	}
	a, err := store.SaveGatewayRouteAs(ctx, owner, route, nil, secrets)
	if err != nil || a.ID == "" || a.State != "enabled" || a.Weight != 1 {
		t.Fatalf("create route: %+v %v", a, err)
	}
	// A personal subscription is never a route, whoever asks.
	personal := GatewayRoute{Name: "members-claude", Kind: "anthropic", ConnectionID: "member-claude"}
	if _, err := store.SaveGatewayRouteAs(ctx, owner, personal, nil, secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("personal subscription became a route: %v", err)
	}
	// A key of another provider cannot back this kind.
	if _, err := store.SaveGatewayRouteAs(ctx, owner, GatewayRoute{Name: "wrong", Kind: "anthropic", ConnectionID: "org-openai"}, nil, secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("openai key as anthropic route: %v", err)
	}
	// A Bedrock route stores its AWS key as a new organization connection.
	bedrock := GatewayRoute{Name: "bedrock-use1", Kind: "bedrock", Region: "us-east-1", Priority: 2,
		ModelMap: map[string]string{"claude-opus-5-5": "us.anthropic.claude-opus-5-5-v1:0"}}
	if _, err := store.SaveGatewayRouteAs(ctx, owner, bedrock, nil, secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cloud route without a credential: %v", err)
	}
	credential := &CloudCredential{Secret: []byte(`{"access_key_id":"AKIAEXAMPLEEXAMPLE01","secret_access_key":"test-only"}`), ExternalAccount: "AKIAEXAMPLEEXAMPLE01"}
	b, err := store.SaveGatewayRouteAs(ctx, owner, bedrock, credential, secrets)
	if err != nil || b.ConnectionID == "" {
		t.Fatalf("bedrock route: %+v %v", b, err)
	}
	var method, ownerKind string
	if err := pool.QueryRow(ctx, `SELECT auth_method,owner_kind FROM access_connections WHERE organization_id=$1 AND id=$2`,
		org, b.ConnectionID).Scan(&method, &ownerKind); err != nil || method != "aws_sigv4" || ownerKind != "organization" {
		t.Fatalf("bedrock connection: %s %s %v", method, ownerKind, err)
	}
	// Updating keeps kind and connection; a credential on update rotates it.
	b.Priority, b.Kind, b.ConnectionID = 5, "anthropic", "org-anthropic"
	if b, err = store.SaveGatewayRouteAs(ctx, owner, b, credential, secrets); err != nil || b.Kind != "bedrock" || b.Priority != 5 {
		t.Fatalf("update bedrock route: %+v %v", b, err)
	}
	var version int64
	if err := pool.QueryRow(ctx, `SELECT active_secret_version FROM access_connections WHERE organization_id=$1 AND id=$2`,
		org, b.ConnectionID).Scan(&version); err != nil || version != 2 {
		t.Fatalf("rotated version: %d %v", version, err)
	}

	p, err := store.SaveGatewayPoolAs(ctx, owner, GatewayPool{Name: "Claude production", Family: "anthropic",
		RouteIDs: []string{a.ID, b.ID}, ProjectIDs: []string{project}})
	if err != nil || p.ID == "" || p.Strategy != "priority_headroom" {
		t.Fatalf("pool: %+v %v", p, err)
	}
	if _, err := store.SaveGatewayPoolAs(ctx, owner, GatewayPool{Name: "GPT", Family: "openai", RouteIDs: []string{a.ID}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("anthropic route in an openai pool: %v", err)
	}
	// Dropping the project revokes its grant.
	p.ProjectIDs = nil
	if _, err := store.SaveGatewayPoolAs(ctx, owner, p); err != nil {
		t.Fatal(err)
	}
	if err := store.SetGatewayRouteStateAs(ctx, owner, a.ID, "draining"); err != nil {
		t.Fatal(err)
	}
	view, err := store.GatewayRoutesAs(ctx, owner)
	if err != nil || len(view.Routes) != 2 || len(view.Pools) != 1 || len(view.Pools[0].RouteIDs) != 2 ||
		len(view.Pools[0].ProjectIDs) != 0 || len(view.Connections) != 2 || len(view.Projects) != 1 {
		t.Fatalf("view: %+v %v", view, err)
	}
	for _, r := range view.Routes {
		if r.ID == a.ID && (r.State != "draining" || len(r.PoolIDs) != 1) {
			t.Fatalf("route a: %+v", r)
		}
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1 AND action LIKE 'gateway.%'`,
		org).Scan(&audited); err != nil || audited < 5 {
		t.Fatalf("audit events: %d %v", audited, err)
	}

	// Subscription meters: each owner sees only their own.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_subscription_limits (organization_id,connection_id,principal_id,window_name,used_pct,window_minutes)
		VALUES ($1,'member-claude',$2,'claude_5h',40,300)`, org, member.PrincipalID); err != nil {
		t.Fatal(err)
	}
	mine, _, err := store.MySubscriptionLimitsAs(ctx, member)
	if err != nil || len(mine) != 1 || mine[0].ConnectionID != "member-claude" || len(mine[0].Windows) != 1 || mine[0].Windows[0].UsedPct != 40 {
		t.Fatalf("member limits: %+v %v", mine, err)
	}
	theirs, _, err := store.MySubscriptionLimitsAs(ctx, other)
	if err != nil || len(theirs) != 1 || theirs[0].ConnectionID != "other-codex" || len(theirs[0].Windows) != 0 {
		t.Fatalf("other limits: %+v %v", theirs, err)
	}
	if admin, _, err := store.MySubscriptionLimitsAs(ctx, owner); err != nil || len(admin) != 0 {
		t.Fatalf("owner saw members' subscriptions: %+v %v", admin, err)
	}
}
