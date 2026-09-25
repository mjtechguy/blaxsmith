package workflow

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func TestBudgetForecastAndThresholds(t *testing.T) {
	// 10 days into a 30-day month: $10 so far projects to $30.
	now := time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC)
	if got := BudgetForecast(10_000_000, now); got != 30_000_000 {
		t.Fatalf("forecast = %d", got)
	}
	// The first hours count as one whole day.
	if got := BudgetForecast(1_000_000, time.Date(2026, 9, 1, 2, 0, 0, 0, time.UTC)); got != 30_000_000 {
		t.Fatalf("early forecast = %d", got)
	}
	start, end := BudgetPeriod(time.Date(2026, 12, 31, 23, 0, 0, 0, time.FixedZone("x", -5*3600)))
	if start.Format("2006-01-02") != "2027-01-01" || end.Format("2006-01-02") != "2027-02-01" {
		t.Fatalf("period is the UTC month: %v %v", start, end)
	}
	for _, c := range []struct {
		spend int64
		want  int32
	}{{0, 0}, {49, 0}, {50, 50}, {85, 80}, {100, 100}, {250, 100}} {
		if got := crossedThreshold(c.spend, 100, DefaultBudgetThresholds); got != c.want {
			t.Fatalf("crossed(%d) = %d, want %d", c.spend, got, c.want)
		}
	}
	if got, ok := normalizeThresholds([]int32{100, 50}); !ok || !slices.Equal(got, []int32{50, 100}) {
		t.Fatalf("thresholds sorted: %v %v", got, ok)
	}
	for _, bad := range [][]int32{{0}, {201}, {50, 50}, {10, 20, 30, 40, 50, 60, 70}} {
		if _, ok := normalizeThresholds(bad); ok {
			t.Fatalf("accepted thresholds %v", bad)
		}
	}
}

func setDailySpend(t *testing.T, pool *pgxpool.Pool, org, project, principal string, micros int64) {
	t.Helper()
	day := time.Now().UTC().Format("2006-01-02")
	if _, err := pool.Exec(t.Context(), `INSERT INTO gateway_usage_daily (organization_id,day,project_id,principal_id,pool_id,route_id,
		route_kind,model,requests,errors,rate_limited,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,
		reasoning_tokens,cost_usd_micros,ttft_ms_sum,ttft_count)
		VALUES ($1,$2,$3,$4,'','c','anthropic','claude-opus-5-5',1,0,0,10,10,0,0,0,$5,0,0)
		ON CONFLICT (organization_id,day,project_id,principal_id,pool_id,route_id,model) DO UPDATE SET cost_usd_micros=EXCLUDED.cost_usd_micros`,
		org, day, project, principal, micros); err != nil {
		t.Fatal(err)
	}
}

func inboxAlerts(t *testing.T, store *Store, caller identity.Caller) []InboxItem {
	t.Helper()
	items, _, err := store.ListInbox(t.Context(), caller, InboxFilter{Kinds: []string{"budget_alert"}})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestBudgetsAlertsExactlyOnce(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	org := organization(t, pool, "gw-budgets")
	owner := reviewer(t, pool, org, "owner", "gwb-owner")
	lead := reviewer(t, pool, org, "member", "gwb-lead") // creates the project, so administers it.
	other := reviewer(t, pool, org, "member", "gwb-other")
	project, err := store.CreateProjectAs(ctx, lead, "gwb-project", "Budget project")
	if err != nil {
		t.Fatal(err)
	}

	// Budget administration is owners and admins only, validated and audited.
	in := BudgetInput{Name: "Budget project monthly", Scope: "project", ProjectID: project, AmountUSDMicros: 100_000_000}
	if _, err := store.CreateBudgetAs(ctx, lead, in); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member created a budget: %v", err)
	}
	for _, bad := range []BudgetInput{{Name: "x", Scope: "project", AmountUSDMicros: 1}, {Name: "", Scope: "organization", AmountUSDMicros: 1},
		{Name: "x", Scope: "organization", AmountUSDMicros: 0}, {Name: "x", Scope: "team", AmountUSDMicros: 1},
		{Name: "x", Scope: "organization", AmountUSDMicros: 1, Thresholds: []int32{0}}} {
		if _, err := store.CreateBudgetAs(ctx, owner, bad); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid budget %+v: %v", bad, err)
		}
	}
	budget, err := store.CreateBudgetAs(ctx, owner, in)
	if err != nil || !slices.Equal(budget.Thresholds, DefaultBudgetThresholds) || budget.ProjectName != "Budget project" || budget.Version != 1 {
		t.Fatalf("create: %+v %v", budget, err)
	}
	if _, err := store.CreateBudgetAs(ctx, owner, in); !errors.Is(err, ErrBudgetExists) {
		t.Fatalf("second active project budget: %v", err)
	}
	orgBudget, err := store.CreateBudgetAs(ctx, owner, BudgetInput{Name: "Org", Scope: "organization", AmountUSDMicros: 1_000_000_000, Thresholds: []int32{90}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateBudgetAs(ctx, owner, orgBudget.ID, BudgetInput{Name: "Org", AmountUSDMicros: 2}, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	if updated, err := store.UpdateBudgetAs(ctx, owner, orgBudget.ID, BudgetInput{Name: "Whole org", AmountUSDMicros: 900_000_000, Thresholds: []int32{90}}, 1); err != nil ||
		updated.Name != "Whole org" || updated.Version != 2 || updated.Scope != "organization" {
		t.Fatalf("update: %+v %v", updated, err)
	}
	if err := store.ArchiveBudgetAs(ctx, owner, orgBudget.ID); err != nil {
		t.Fatal(err)
	}

	// Spend at 60%: nothing fires while the gateway or the budgets switch is off.
	setDailySpend(t, pool, org, project, lead.PrincipalID, 60_000_000)
	if n, err := EvaluateBudgets(ctx, pool, time.Now()); err != nil || n != 0 {
		t.Fatalf("evaluated while off: %d %v", n, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_org_settings (organization_id,enabled) VALUES ($1,true)`, org); err != nil {
		t.Fatal(err)
	}
	if n, _ := EvaluateBudgets(ctx, pool, time.Now()); n != 0 {
		t.Fatalf("fired with budgets switched off: %d", n)
	}
	if _, err := store.SetBudgetsEnabledAs(ctx, lead, true); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member switched budgets on: %v", err)
	}
	if on, err := store.SetBudgetsEnabledAs(ctx, owner, true); err != nil || !on {
		t.Fatal(err)
	}
	if n, err := EvaluateBudgets(ctx, pool, time.Now()); err != nil || n != 1 {
		t.Fatalf("50%% crossing: %d %v", n, err)
	}

	// Crossing 80% under concurrent rollup ticks: exactly one alert and one audit event.
	setDailySpend(t, pool, org, project, lead.PrincipalID, 85_000_000)
	var wg sync.WaitGroup
	fired := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := EvaluateBudgets(ctx, pool, time.Now())
			if err != nil {
				t.Error(err)
			}
			fired <- n
		}()
	}
	wg.Wait()
	close(fired)
	total := 0
	for n := range fired {
		total += n
	}
	var alerts80, audits80 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway_alerts WHERE organization_id=$1 AND threshold_pct=80`, org).Scan(&alerts80); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND action='gateway.budget.threshold_crossed' AND (detail->>'threshold_pct')::int=80 AND project_id=$2`, org, project).Scan(&audits80); err != nil {
		t.Fatal(err)
	}
	if total != 1 || alerts80 != 1 || audits80 != 1 {
		t.Fatalf("80%% crossing: fired %d, alerts %d, audit events %d", total, alerts80, audits80)
	}
	if n, _ := EvaluateBudgets(ctx, pool, time.Now()); n != 0 {
		t.Fatalf("re-fired on the next tick: %d", n)
	}

	// The inbox shows the open alerts to their recipients only.
	ownerItems := inboxAlerts(t, store, owner)
	leadItems := inboxAlerts(t, store, lead)
	if len(ownerItems) != 2 || len(leadItems) != 2 || len(inboxAlerts(t, store, other)) != 0 {
		t.Fatalf("inbox recipients: owner %d, project admin %d, other %d", len(ownerItems), len(leadItems), len(inboxAlerts(t, store, other)))
	}
	eighty := leadItems[0]
	for _, item := range leadItems {
		if strings.Contains(item.Title, "80%") {
			eighty = item
		}
	}
	if eighty.Kind != "budget_alert" || eighty.RunID != "" || eighty.ProjectID != project || eighty.Stage != "project" || eighty.Target != "project_usage" ||
		eighty.Blocking || !eighty.CanAct || !strings.Contains(eighty.Title, "Budget project monthly passed 80%") {
		t.Fatalf("inbox item: %+v", eighty)
	}
	if err := store.AcknowledgeBudgetAlertAs(ctx, other, eighty.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-recipient acknowledged: %v", err)
	}
	if err := store.AcknowledgeBudgetAlertAs(ctx, lead, eighty.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.AcknowledgeBudgetAlertAs(ctx, owner, eighty.ID); err != nil {
		t.Fatalf("acknowledging twice is a no-op: %v", err)
	}
	if items := inboxAlerts(t, store, owner); len(items) != 1 || items[0].ID == eighty.ID {
		t.Fatalf("acknowledged alert still in the inbox: %+v", items)
	}
	if _, err := store.SnoozeBudgetAlertAs(ctx, owner, ownerItems[0].ID, 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero-hour snooze: %v", err)
	}
	var fifty string
	for _, item := range ownerItems {
		if item.ID != eighty.ID {
			fifty = item.ID
		}
	}
	if _, err := store.SnoozeBudgetAlertAs(ctx, owner, fifty, 24); err != nil {
		t.Fatal(err)
	}
	if items := inboxAlerts(t, store, owner); len(items) != 0 {
		t.Fatalf("snoozed alert still in the inbox: %+v", items)
	}

	// Admin → Alerts feed and Admin → Budgets progress.
	feed, err := store.BudgetAlertsAs(ctx, owner, false)
	if err != nil || len(feed) != 2 || feed[0].ThresholdPct != 80 || feed[0].AcknowledgedAt == nil || feed[0].AcknowledgedBy != "gwb-lead" ||
		feed[1].SnoozedUntil == nil || feed[0].SpendUSDMicros != 85_000_000 || feed[0].BudgetName != "Budget project monthly" {
		t.Fatalf("alert feed: %+v %v", feed, err)
	}
	if open, _ := store.BudgetAlertsAs(ctx, owner, true); len(open) != 1 {
		t.Fatalf("open alerts: %+v", open)
	}
	if _, err := store.BudgetAlertsAs(ctx, lead, false); !errors.Is(err, ErrAdminDenied) {
		t.Fatalf("member read the alert feed: %v", err)
	}
	view, err := store.BudgetsAs(ctx, owner)
	if err != nil || !view.BudgetsEnabled || !view.GatewayEnabled || len(view.Budgets) != 1 ||
		view.Budgets[0].SpendUSDMicros != 85_000_000 || !slices.Equal(view.Budgets[0].FiredThresholds, []int32{50, 80}) ||
		view.Budgets[0].ForecastUSDMicros < 85_000_000 {
		t.Fatalf("budgets view: %+v %v", view, err)
	}

	// Project → Usage: any member; spend by user for project administrators only.
	usage, err := store.ProjectUsageAs(ctx, other, project)
	if err != nil || !usage.Enabled || usage.Totals.CostUSDMicro != 85_000_000 || usage.Budget == nil || usage.Budget.ID != budget.ID ||
		!usage.ByUserHidden || len(usage.ByUser) != 0 || len(usage.ByModel) != 1 || len(usage.Alerts) != 2 || len(usage.Series) != 1 {
		t.Fatalf("member project usage: %+v %v", usage, err)
	}
	if usage, err := store.ProjectUsageAs(ctx, lead, project); err != nil || usage.ByUserHidden || len(usage.ByUser) != 1 ||
		usage.ByUser[0].Key != lead.PrincipalID {
		t.Fatalf("project admin usage: %+v %v", usage, err)
	}

	// A user budget reaches only that user (and owners and admins).
	userBudget, err := store.CreateBudgetAs(ctx, owner, BudgetInput{Name: "Other's spend", Scope: "user", PrincipalID: other.PrincipalID,
		AmountUSDMicros: 1_000_000, Thresholds: []int32{100}})
	if err != nil {
		t.Fatal(err)
	}
	setDailySpend(t, pool, org, project, other.PrincipalID, 2_000_000)
	if n, err := EvaluateBudgets(ctx, pool, time.Now()); err != nil || n != 1 {
		t.Fatalf("user budget: %d %v", n, err)
	}
	// The budget's own user opens My usage; owners and admins open the admin feed.
	if items := inboxAlerts(t, store, other); len(items) != 1 || items[0].Stage != "user" || items[0].ProjectID != "" || items[0].Target != "my_usage" {
		t.Fatalf("user alert: %+v", items)
	}
	var ownerView []InboxItem
	for _, item := range inboxAlerts(t, store, owner) {
		if item.Stage == "user" {
			ownerView = append(ownerView, item)
		}
	}
	if len(ownerView) != 1 || ownerView[0].Target != "admin_alerts" {
		t.Fatalf("owner's view of another user's budget alert: %+v", ownerView)
	}
	if n := len(inboxAlerts(t, store, lead)); n != 0 {
		t.Fatalf("project admin sees a user budget alert: %d", n)
	}
	_ = userBudget
}

func TestRunCostPersonalSubscriptionPrivacy(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	org := organization(t, pool, "gw-privacy")
	admin := reviewer(t, pool, org, "admin", "gwp-admin")
	alice := reviewer(t, pool, org, "member", "gwp-alice")
	bob := reviewer(t, pool, org, "member", "gwp-bob")
	project, err := store.CreateProject(ctx, org, "gwp-project", "Privacy project")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "personal-run",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "implement", strings.Repeat("d", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if _, err := pool.Exec(ctx, `INSERT INTO gateway_usage_events (organization_id,project_id,run_id,attempt_id,task_id,stage_key,
			principal_id,harness,route_id,route_kind,api,requested_model,status,http_status,started_at,input_tokens,output_tokens,cost_usd_micros)
			VALUES ($1,$2,$3,$4,$5,'implement',$6,'codex','c','openai','openai_responses','gpt-6-luna','ok',200,
			clock_timestamp()-make_interval(secs => $7),100,10,1000000)`, org, project, run.ID, attempt.ID, task, alice.PrincipalID, i); err != nil {
			t.Fatal(err)
		}
	}
	// An organization connection: every member sees per-request detail.
	if cost, err := store.RunCostAs(ctx, bob, run.ID); err != nil || cost.CallsRestricted || len(cost.Calls) != 2 {
		t.Fatalf("org-connection run: %+v %v", cost, err)
	}
	// Bind the attempt to Alice's personal subscription connection.
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,'reg','openai','https://api.openai.com',ARRAY['native_raw'],'active')`, []any{org}},
		{`INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,'personal','user',$2,'reg','acct','oauth','active')`, []any{org, alice.PrincipalID}},
		{`INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ($1,$2,1,false,ARRAY['native_raw'])`, []any{org, project}},
		{`INSERT INTO access_grants (organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ($1,'grant','personal',$2,'user',$3,'model.invoke','openai/gpt-6-luna','native_raw',$3)`, []any{org, project, alice.PrincipalID}},
		{`INSERT INTO access_bindings (organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,policy_version)
		VALUES ($1,'binding',$2,$3,'grant',1,'model.invoke','openai/gpt-6-luna',1)`, []any{org, attempt.ID, project}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	// Another member: totals and stage aggregates, never per-request rows.
	cost, err := store.RunCostAs(ctx, bob, run.ID)
	if err != nil || !cost.CallsRestricted || len(cost.Calls) != 0 || len(cost.Stages) != 1 ||
		cost.Stages[0].Totals.Requests != 2 || cost.Stages[0].Totals.CostUSDMicro != 2_000_000 {
		t.Fatalf("non-owner view: %+v %v", cost, err)
	}
	// The owner and organization admins see the detail.
	for _, caller := range []identity.Caller{alice, admin} {
		if cost, err := store.RunCostAs(ctx, caller, run.ID); err != nil || cost.CallsRestricted || len(cost.Calls) != 2 {
			t.Fatalf("%s view: %+v %v", caller.Role, cost, err)
		}
	}
}
