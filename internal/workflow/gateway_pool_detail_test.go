package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// TestGatewayAzureRouteAndPoolDetail: an Azure OpenAI route stores its key
// as a route-only organization connection, joins an OpenAI pool, and the
// pool's Traffic and Failovers tabs are built from its usage events.
func TestGatewayAzureRouteAndPoolDetail(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "gw-detail")
	owner := reviewer(t, pool, org, "owner", "gw-detail-owner")
	member := reviewer(t, pool, org, "member", "gw-detail-member")
	project, err := store.CreateProjectAs(ctx, member, "gw-detail-project", "Detail project")
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := access.NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,'reg-openai','openai','https://api.openai.com',ARRAY['native_raw'],'active')`, org); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,
		external_account_id,auth_method,state,label) VALUES ($1,'org-openai','organization',$1,'reg-openai','acct','api_key','active','OpenAI key')`, org); err != nil {
		t.Fatal(err)
	}
	azure := GatewayRoute{Name: "azure-eastus", Kind: "azure_openai", AzureResource: "contoso-ai", APIVersion: "2024-10-21",
		Priority: 2, ModelMap: map[string]string{"gpt-6-luna": "luna-prod"}}
	credential := &CloudCredential{Secret: []byte("azure-test-key-0123456789"), ExternalAccount: "azure-contoso-ai"}
	if _, err := store.SaveGatewayRouteAs(ctx, owner, GatewayRoute{Name: "bad", Kind: "azure_openai", AzureResource: "https://evil",
		APIVersion: "2024-10-21", ModelMap: azure.ModelMap}, credential, secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("URL accepted as an Azure resource: %v", err)
	}
	az, err := store.SaveGatewayRouteAs(ctx, owner, azure, credential, secrets)
	if err != nil {
		t.Fatal(err)
	}
	var method string
	if err := pool.QueryRow(ctx, `SELECT auth_method FROM access_connections WHERE organization_id=$1 AND id=$2`, org, az.ConnectionID).Scan(&method); err != nil || method != "azure_api_key" {
		t.Fatalf("azure connection: %q %v", method, err)
	}
	direct, err := store.SaveGatewayRouteAs(ctx, owner, GatewayRoute{Name: "openai-a", Kind: "openai", ConnectionID: "org-openai", Priority: 1}, nil, secrets)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.SaveGatewayPoolAs(ctx, owner, GatewayPool{Name: "GPT production", Family: "openai", RouteIDs: []string{direct.ID, az.ID},
		ProjectIDs: []string{project}})
	if err != nil {
		t.Fatalf("openai pool with an Azure route: %v", err)
	}
	// One request failed over (429 on the key, served by Azure), one plain.
	for _, e := range []struct {
		route, status string
		code, retry   int
		attempt       string
		offset        string
	}{
		{direct.ID, "error", 429, 0, "a1", "0 seconds"},
		{az.ID, "ok", 200, 1, "a1", "1 second"},
		{direct.ID, "ok", 200, 0, "a2", "5 seconds"},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO gateway_usage_events (organization_id,project_id,run_id,attempt_id,task_id,stage_key,harness,
			pool_id,route_id,route_kind,api,status,http_status,retry_count,started_at,input_tokens,cost_usd_micros)
			VALUES ($1,$2,md5('run')::uuid,md5($3)::uuid,md5('task')::uuid,'implement','codex',$4,$5,'openai','openai_chat',$6,$7,$8,
				clock_timestamp()-interval '1 hour'+$9::interval,100,50)`,
			org, project, e.attempt, p.ID, e.route, e.status, e.code, e.retry, e.offset); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.GatewayPoolDetailAs(ctx, member, p.ID, 24); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member read pool detail: %v", err)
	}
	d, err := store.GatewayPoolDetailAs(ctx, owner, p.ID, 24)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Traffic) != 2 || d.Traffic[0].RouteID != direct.ID || d.Traffic[0].Requests != 2 || d.Traffic[0].RateLimited != 1 ||
		d.Traffic[0].FailoversFrom != 1 || d.Traffic[1].Requests != 1 || d.Traffic[1].Tokens != 100 {
		t.Fatalf("traffic: %+v", d.Traffic)
	}
	if len(d.Failovers) != 1 || d.Failovers[0].FromRouteName != "openai-a" || d.Failovers[0].ToRouteName != "azure-eastus" ||
		d.Failovers[0].HTTPStatus != 429 || d.Failovers[0].FinalStatus != "ok" || d.Failovers[0].Stage != "implement" {
		t.Fatalf("failovers: %+v", d.Failovers)
	}
	if short, _ := store.GatewayPoolDetailAs(ctx, owner, p.ID, 0); short.Hours != 24 {
		t.Fatalf("default window: %d", short.Hours)
	}
}
