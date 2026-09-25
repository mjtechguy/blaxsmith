package bootstrap

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/gateway"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// TestClaudeSetupTokenThroughGatewayPostgres: a member's own Claude
// setup-token in a brokered_gateway attempt stays on the platform. The
// sandbox receives only a gateway token; the gateway sends the setup-token
// upstream as an OAuth bearer with the OAuth beta, for the owner's own run.
func TestClaudeSetupTokenThroughGatewayPostgres(t *testing.T) {
	const owner = "00000000-0000-4000-8000-00000000a11c"
	const setupToken = "sk-ant-oat01-owner-only-test-token"
	pool, secrets, model, token, err := gatewayReleaseWith(t, true, modelFixtureSpec{
		provider: "anthropic", model: "claude-opus-5-5", origin: "https://api.anthropic.com",
		authMethod: access.ClaudeSetupTokenAuth, key: setupToken,
		ownerKind: "user", owner: owner, granteeKind: "user", grantee: owner}, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, gateway.TokenPrefix) || strings.Contains(token, "sk-ant-oat") {
		t.Fatalf("sandbox received %q, want a gateway token", token)
	}
	authorizer := &gateway.Authorizer{DB: pool, Secrets: secrets}
	grant, err := authorizer.Authorize(t.Context(), token, "anthropic")
	if err != nil || grant.AuthMethod != access.ClaudeSetupTokenAuth || grant.OwnerID != owner || string(grant.Key) != setupToken {
		t.Fatalf("authorize: %+v %v", grant, err)
	}
	clear(grant.Key)

	var seen http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.4")
		_, _ = io.WriteString(w, `{"model":"claude-opus-5-5","usage":{"input_tokens":3,"output_tokens":4}}`)
	}))
	defer upstream.Close()
	server := &gateway.Server{DB: pool, Authorize: authorizer.Authorize, Prices: &gateway.PriceBook{DB: pool},
		Upstream: map[string]string{"anthropic": upstream.URL}}
	front := httptest.NewServer(server)
	defer front.Close()
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":8}`))
	request.Header.Set("Authorization", "Bearer "+token) // Claude Code --bare with ANTHROPIC_AUTH_TOKEN
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || seen.Get("Authorization") != "Bearer "+setupToken || seen.Get("X-Api-Key") != "" ||
		seen.Get("Anthropic-Beta") != "oauth-2025-04-20" {
		t.Fatalf("upstream saw %d %v", response.StatusCode, seen)
	}
	// The owner's subscription window is recorded for the owner only.
	var used float64
	var principal string
	if err := pool.QueryRow(tenant.System(t.Context()), `SELECT used_pct,principal_id::text FROM gateway_subscription_limits
		WHERE organization_id=$1 AND connection_id='connection' AND window_name='claude_5h'`, model.Invoke.OrganizationID).
		Scan(&used, &principal); err != nil || used != 40 || principal != owner {
		t.Fatalf("subscription window: %v %q %v", used, principal, err)
	}
	var routeKind string
	if err := pool.QueryRow(tenant.System(t.Context()), `SELECT route_kind FROM gateway_usage_events WHERE organization_id=$1`,
		model.Invoke.OrganizationID).Scan(&routeKind); err != nil || routeKind != gateway.KindPersonal {
		t.Fatalf("usage route kind %q %v", routeKind, err)
	}
	// Turning off members' own Claude subscriptions stops the very next request.
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE identity_organizations SET allow_member_claude_subscription=false WHERE id=$1`,
		model.Invoke.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := authorizer.Authorize(t.Context(), token, "anthropic"); !errors.Is(err, gateway.ErrDenied) {
		t.Fatalf("setup-token authorized with the org switch off: %v", err)
	}
	// A pooled route can never be a personal subscription.
	if _, err := authorizer.RouteKey(t.Context(), grant, gateway.Route{ID: "r", Kind: gateway.KindAnthropic,
		ConnectionID: "connection", AuthMethod: access.ClaudeSetupTokenAuth}); !errors.Is(err, gateway.ErrDenied) {
		t.Fatalf("personal subscription read as a pool route: %v", err)
	}
	if _, err := authorizer.RouteKey(t.Context(), grant, gateway.Route{ID: "r", Kind: gateway.KindAnthropic,
		ConnectionID: "connection", AuthMethod: "api_key"}); !errors.Is(err, gateway.ErrDenied) {
		t.Fatalf("user-owned connection read as a pool route: %v", err)
	}
}

// TestCodexSignInMintsOnlyAsPersonalRoutePostgres: an owner's Codex sign-in
// in a brokered attempt keeps its native delivery unless the organization
// serves personal subscription routes; then it gets a gateway token.
func TestCodexSignInMintsOnlyAsPersonalRoutePostgres(t *testing.T) {
	pool, _, _, model, _, _ := modelAttemptFixture(t)
	ctx := tenant.System(t.Context())
	org := model.Invoke.OrganizationID
	for _, statement := range []string{
		`UPDATE access_connections SET auth_method='codex_chatgpt' WHERE organization_id=$1`,
		`INSERT INTO gateway_org_settings (organization_id,enabled) VALUES ($1::uuid,true)`,
	} {
		if _, err := pool.Exec(ctx, statement, org); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_attempt_delivery (organization_id,attempt_id,delivery_mode,base_url,harness)
		VALUES ($1,$2,'brokered_gateway','https://gw.example','codex')`, org, model.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	mint := func() bool {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		token, ok, err := gateway.Mint(ctx, tx, gateway.MintRequest{Invoke: model.Invoke, RunID: model.Attempt.RunID,
			TaskID: model.Attempt.TaskID, LeaseID: "lease", OwnerGeneration: 1})
		if err != nil {
			t.Fatal(err)
		}
		return ok && strings.HasPrefix(string(token), gateway.TokenPrefix)
	}
	if mint() {
		t.Fatal("Codex sign-in brokered with personal subscription routes off")
	}
	if _, err := pool.Exec(ctx, `UPDATE gateway_org_settings SET personal_routes_enabled=true WHERE organization_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	if !mint() {
		t.Fatal("Codex sign-in not brokered as a personal route")
	}
}
