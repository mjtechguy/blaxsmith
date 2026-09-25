package workflow

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestGatewaySettingsAndProjectDelivery(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "gw-settings")
	owner := reviewer(t, pool, org, "owner", "gw-owner")
	member := reviewer(t, pool, org, "member", "gw-member")
	other := reviewer(t, pool, org, "member", "gw-other")
	project, err := store.CreateProjectAs(ctx, member, "gw-project", "Gateway project")
	if err != nil {
		t.Fatal(err)
	}
	view, err := store.GatewaySettingsAs(ctx, owner)
	if err != nil || view.Version != 0 || view.Settings != defaultGatewaySettings {
		t.Fatalf("defaults: %+v %v", view, err)
	}
	if _, err := store.GatewaySettingsAs(ctx, member); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member read admin settings: %v", err)
	}
	on := GatewaySettings{Enabled: true, DefaultDeliveryMode: "brokered_gateway", AllowProjectChoice: true, RemoveDirectEgress: true}
	if _, err := store.UpdateGatewaySettingsAs(ctx, owner, on, 0, false); !errors.Is(err, ErrGatewayUnavailable) {
		t.Fatalf("enabled without a gateway Deployment: %v", err)
	}
	if _, err := store.UpdateGatewaySettingsAs(ctx, member, on, 0, true); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member changed settings: %v", err)
	}
	saved, err := store.UpdateGatewaySettingsAs(ctx, owner, on, 0, true)
	if err != nil || saved.Version != 1 {
		t.Fatalf("enable: %+v %v", saved, err)
	}
	if _, err := store.UpdateGatewaySettingsAs(ctx, owner, defaultGatewaySettings, 0, true); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail::text FROM identity_audit_events WHERE organization_id=$1
		AND action='gateway.settings.updated' ORDER BY id DESC LIMIT 1`, org).Scan(&detail); err != nil ||
		!strings.Contains(detail, `"old": {"enabled": false`) || !strings.Contains(detail, `"new": {"enabled": true`) {
		t.Fatalf("audit old → new: %s %v", detail, err)
	}
	enabled, _ := store.GatewayEnabled(ctx, org)
	if !enabled {
		t.Fatal("master switch not on")
	}

	d, err := store.ProjectDeliveryAs(ctx, other, project)
	if err != nil || d.Effective != "brokered_gateway" || d.CanEdit || d.ProjectChoice != "" {
		t.Fatalf("member view of project delivery: %+v %v", d, err)
	}
	if _, err := store.SetProjectDeliveryAs(ctx, other, project, "native_raw"); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("non-admin changed project delivery: %v", err)
	}
	d, err = store.SetProjectDeliveryAs(ctx, member, project, "native_raw")
	if err != nil || d.Effective != "native_raw" || d.ProjectChoice != "native_raw" || !d.CanEdit {
		t.Fatalf("project admin choice: %+v %v", d, err)
	}
	// The org enforces its default: the choice is ignored and cannot change.
	enforce := on
	enforce.AllowProjectChoice = false
	if _, err := store.UpdateGatewaySettingsAs(ctx, owner, enforce, 1, true); err != nil {
		t.Fatal(err)
	}
	if d, _ := store.ProjectDeliveryAs(ctx, member, project); d.Effective != "brokered_gateway" {
		t.Fatalf("enforced default: %+v", d)
	}
	if _, err := store.SetProjectDeliveryAs(ctx, member, project, "native_raw"); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("choice changed while enforced: %v", err)
	}
	// Master switch off: every project falls back to native_raw.
	off := enforce
	off.Enabled = false
	if _, err := store.UpdateGatewaySettingsAs(ctx, owner, off, 2, false); err != nil {
		t.Fatalf("turning off never needs the Deployment: %v", err)
	}
	if d, _ := store.ProjectDeliveryAs(ctx, member, project); d.Effective != "native_raw" || d.GatewayEnabled {
		t.Fatalf("disabled gateway: %+v", d)
	}
}

func TestGatewayUsageReads(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "gw-usage")
	owner := reviewer(t, pool, org, "owner", "gwu-owner")
	alice := reviewer(t, pool, org, "member", "gwu-alice")
	bob := reviewer(t, pool, org, "member", "gwu-bob")
	project, err := store.CreateProjectAs(ctx, alice, "gwu-project", "Usage project")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "cost-run",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "implement", strings.Repeat("d", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().UTC().Format("2006-01-02")
	for _, row := range []struct {
		principal, model string
		cost             int64
	}{{alice.PrincipalID, "claude-opus-5-5", 5_000_000}, {alice.PrincipalID, "gpt-6-luna", 1_000_000}, {bob.PrincipalID, "claude-opus-5-5", 9_000_000}} {
		if _, err := pool.Exec(ctx, `INSERT INTO gateway_usage_daily (organization_id,day,project_id,principal_id,pool_id,route_id,
			route_kind,model,requests,errors,rate_limited,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,
			reasoning_tokens,cost_usd_micros,ttft_ms_sum,ttft_count)
			VALUES ($1,$2,$3,$4,'','c','anthropic',$5,2,0,0,100,10,50,0,0,$6,0,0)`, org, today, project, row.principal, row.model, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_run_usage (organization_id,run_id,project_id,principal_id,requests,
		input_tokens,output_tokens,cache_read_tokens,cost_usd_micros,last_request_at)
		VALUES ($1,$2,$3,$4,2,100,10,300,6000000,clock_timestamp())`, org, run.ID, project, alice.PrincipalID); err != nil {
		t.Fatal(err)
	}
	for i, stage := range []string{"implement", "implement"} {
		if _, err := pool.Exec(ctx, `INSERT INTO gateway_usage_events (organization_id,project_id,run_id,attempt_id,task_id,stage_key,
			principal_id,harness,route_id,route_kind,api,requested_model,status,http_status,started_at,ttft_ms,input_tokens,
			output_tokens,cache_read_tokens,cost_usd_micros)
			VALUES ($1,$2,$3,gen_random_uuid(),$4,$5,$6,'claude-code','c','anthropic','anthropic_messages','claude-opus-5-5',
			'ok',200,clock_timestamp()-make_interval(secs => $7),400,50,5,150,3000000)`,
			org, project, run.ID, task, stage, alice.PrincipalID, i); err != nil {
			t.Fatal(err)
		}
	}
	mine, err := store.MyUsageAs(ctx, alice, 30)
	if err != nil || mine.Totals.CostUSDMicro != 6_000_000 || len(mine.ByModel) != 2 || mine.ByModel[0].Key != "claude-opus-5-5" ||
		len(mine.ByProject) != 1 || mine.ByProject[0].Label != "Usage project" || len(mine.TopRuns) != 1 || mine.TopRuns[0].Label != "cost-run" {
		t.Fatalf("my usage: %+v %v", mine, err)
	}
	if theirs, _ := store.MyUsageAs(ctx, bob, 30); theirs.Totals.CostUSDMicro != 9_000_000 || len(theirs.TopRuns) != 0 {
		t.Fatalf("bob sees only his own usage: %+v", theirs)
	}
	overview, err := store.UsageOverviewAs(ctx, owner, 7, "model")
	if err != nil || overview.Totals.CostUSDMicro != 15_000_000 || len(overview.TopUsers) != 2 || overview.TopUsers[0].Key != bob.PrincipalID ||
		overview.MedianTTFTMS != 400 || len(overview.Series) != 2 || len(overview.TopRuns) != 1 {
		t.Fatalf("overview: %+v %v", overview, err)
	}
	if _, err := store.UsageOverviewAs(ctx, alice, 7, "project"); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member read the org overview: %v", err)
	}
	cost, err := store.RunCostAs(ctx, bob, run.ID) // any member can view a run in their organization.
	if err != nil || cost.Totals.CostUSDMicro != 6_000_000 || len(cost.Stages) != 1 || cost.Stages[0].Totals.Requests != 2 ||
		len(cost.Calls) != 2 || !cost.Calls[0].StartedAt.After(cost.Calls[1].StartedAt) {
		t.Fatalf("run cost: %+v %v", cost, err)
	}
	prices, err := store.ModelPricesAs(ctx, owner)
	if err != nil || len(prices) == 0 {
		t.Fatalf("prices: %v", err)
	}
	override, err := store.SetModelPriceOverrideAs(ctx, owner, ModelPrice{Provider: "anthropic", Model: "claude-opus-5-5",
		Input: 3_000_000, Output: 15_000_000, CacheRead: 100_000, CacheWrite: 3_750_000})
	if err != nil || override.Source != "override" {
		t.Fatalf("override: %+v %v", override, err)
	}
	if _, err := store.SetModelPriceOverrideAs(ctx, alice, override); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member set a price: %v", err)
	}
	prices, _ = store.ModelPricesAs(ctx, owner)
	for _, p := range prices {
		if p.Model == "claude-opus-5-5" && (p.Source != "override" || p.Input != 3_000_000) {
			t.Fatalf("override not effective: %+v", p)
		}
	}
}
