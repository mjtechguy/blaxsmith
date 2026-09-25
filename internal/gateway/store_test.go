package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL for PostgreSQL gateway tests")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	schema := "blaxsmith_gateway_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

type fixture struct{ org, principal, project, run, task, attempt string }

func newFixture(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := t.Context()
	var f fixture
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name) VALUES (gen_random_uuid(),'gw-org','Gateway')
		RETURNING id`).Scan(&f.org); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO identity_principals (id,username) VALUES (gen_random_uuid(),'alice')
		RETURNING id`).Scan(&f.principal); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	if f.project, err = store.CreateProject(ctx, f.org, "gw-project", "Gateway project"); err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: f.org, ProjectID: f.project, LaunchKey: "gw",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	f.run = run.ID
	if f.task, err = store.AddTask(ctx, f.org, f.run, "implement", strings.Repeat("d", 64), 1); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, f.org, f.run); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, f.org, f.run, f.task)
	if err != nil {
		t.Fatal(err)
	}
	f.attempt = attempt.ID
	return f
}

func TestEffectiveDeliveryFlagsPostgres(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := t.Context()
	const base = "https://gw.example"
	check := func(want string, url string) Delivery {
		t.Helper()
		d, err := EffectiveDelivery(ctx, pool, f.org, f.project, url)
		if err != nil || d.Mode != want {
			t.Fatalf("delivery = %+v %v, want %s", d, err, want)
		}
		return d
	}
	check(ModeNative, base) // no settings row: the gateway is off by default.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_org_settings (organization_id,enabled,default_delivery_mode)
		VALUES ($1,false,'brokered_gateway')`, f.org); err != nil {
		t.Fatal(err)
	}
	check(ModeNative, base) // master switch off: projects fall back to native_raw.
	if _, err := pool.Exec(ctx, `UPDATE gateway_org_settings SET enabled=true WHERE organization_id=$1`, f.org); err != nil {
		t.Fatal(err)
	}
	if d := check(ModeBrokered, base+"/"); d.BaseURL != base || !d.RemoveDirectEgress {
		t.Fatalf("brokered delivery: %+v", d)
	}
	check(ModeNative, "") // no gateway Deployment on this installation.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_project_settings (organization_id,project_id,delivery_mode)
		VALUES ($1,$2,'native_raw')`, f.org, f.project); err != nil {
		t.Fatal(err)
	}
	check(ModeNative, base) // the project chose native_raw.
	if _, err := pool.Exec(ctx, `UPDATE gateway_org_settings SET allow_project_choice=false WHERE organization_id=$1`, f.org); err != nil {
		t.Fatal(err)
	}
	check(ModeBrokered, base) // the org default is enforced.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordAttemptDelivery(ctx, tx, f.org, f.attempt, "claude-code",
		Delivery{Mode: ModeBrokered, BaseURL: base, RemoveDirectEgress: true}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	frozen, err := AttemptDelivery(ctx, pool, f.org, f.attempt)
	if err != nil || frozen != (Delivery{Mode: ModeBrokered, BaseURL: base, RemoveDirectEgress: true}) {
		t.Fatalf("frozen delivery: %+v %v", frozen, err)
	}
	// Settings changing mid-run do not change a dispatched attempt.
	if _, err := pool.Exec(ctx, `UPDATE gateway_org_settings SET enabled=false WHERE organization_id=$1`, f.org); err != nil {
		t.Fatal(err)
	}
	if again, _ := AttemptDelivery(ctx, pool, f.org, f.attempt); again != frozen {
		t.Fatalf("frozen delivery changed: %+v", again)
	}
}

func TestMeteringRollupsAndPricesPostgres(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := t.Context()
	grant := Grant{OrganizationID: f.org, ProjectID: f.project, RunID: f.run, AttemptID: f.attempt, TaskID: f.task,
		StageKey: "implement", PrincipalID: f.principal, Harness: "claude-code", Provider: "anthropic", Model: "claude-opus-5-5"}
	book := &PriceBook{DB: pool, TTL: time.Millisecond}
	now := time.Now().UTC()
	ttft := 800 * time.Millisecond
	record := func(u Usage, status string, code int, at time.Time) {
		t.Helper()
		e := Event{Grant: grant, RouteID: "connection", RouteKind: "anthropic", API: APIAnthropicMessages,
			RequestedModel: "claude-opus-5-5", Status: status, HTTPStatus: code, Streamed: true, StartedAt: at,
			TTFT: &ttft, Duration: 2 * time.Second, Usage: u, Price: book.Lookup(ctx, f.org, "anthropic", "claude-opus-5-5", at)}
		if err := Record(ctx, pool, e); err != nil {
			t.Fatal(err)
		}
	}
	u := Usage{Input: 1_000_000, Output: 100_000, CacheRead: 1_000_000, ServedModel: "claude-opus-5-5", Reported: true}
	record(u, "ok", 200, now)                                // $4 + $2 + $0.20 = $6.20
	record(Usage{}, "error", 429, now.Add(time.Second))      // rate limited, no tokens
	record(u, "ok", 200, now.Add(-24*time.Hour-time.Minute)) // yesterday, clear of the 24h retention edge
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_price_overrides
		(organization_id,provider,model,input_micros_per_mtok,output_micros_per_mtok,cache_read_micros_per_mtok,
		 cache_write_micros_per_mtok,effective_from,created_by)
		VALUES ($1,'anthropic','claude-opus-5-5',2000000,10000000,100000,2500000,$2,$3)`, f.org, now.Add(-time.Minute), f.principal); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	record(u, "ok", 200, now.Add(2*time.Second)) // contracted: $2 + $1 + $0.10 = $3.10
	var requests, errs int64
	var cost int64
	if err := pool.QueryRow(ctx, `SELECT requests,errors,cost_usd_micros FROM gateway_run_usage WHERE organization_id=$1 AND run_id=$2`,
		f.org, f.run).Scan(&requests, &errs, &cost); err != nil || requests != 4 || errs != 1 || cost != 6_200_000*2+3_100_000 {
		t.Fatalf("run totals: requests=%d errors=%d cost=%d %v", requests, errs, cost, err)
	}
	var versions []string
	rows, _ := pool.Query(ctx, `SELECT DISTINCT price_version FROM gateway_usage_events ORDER BY 1`)
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		versions = append(versions, v)
	}
	if len(versions) != 2 || versions[0] != "manifest:2026-09-24" || !strings.HasPrefix(versions[1], "override:") {
		t.Fatalf("price versions: %v", versions)
	}
	for range 2 { // idempotent
		if err := Rollup(ctx, pool, now.Add(-48*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	var days, total, limited, ttftCount int64
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT day),sum(requests),sum(rate_limited),sum(ttft_count)
		FROM gateway_usage_daily WHERE organization_id=$1`, f.org).Scan(&days, &total, &limited, &ttftCount); err != nil ||
		days != 2 || total != 4 || limited != 1 || ttftCount != 4 {
		t.Fatalf("rollup: days=%d requests=%d 429s=%d ttft=%d %v", days, total, limited, ttftCount, err)
	}
	var todayCost int64
	if err := pool.QueryRow(ctx, `SELECT sum(cost_usd_micros) FROM gateway_usage_daily WHERE organization_id=$1 AND day=$2::date`,
		f.org, now.Format("2006-01-02")).Scan(&todayCost); err != nil || todayCost != 6_200_000+3_100_000 {
		t.Fatalf("today's rollup cost: %d %v", todayCost, err)
	}
	if err := EnsurePartitions(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var inDefault int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gateway_usage_events_default`).Scan(&inDefault); err != nil || inDefault != 0 {
		t.Fatalf("events fell into the default partition: %d %v", inDefault, err)
	}
	if err := PruneEvents(ctx, pool, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM gateway_usage_events`).Scan(&left)
	if left != 3 {
		t.Fatalf("retention kept %d events, want 3", left)
	}
	// No prompt or response content is stored anywhere in the events table.
	var columns []string
	rows, _ = pool.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name='gateway_usage_events' AND table_schema=current_schema()`)
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		columns = append(columns, c)
	}
	for _, c := range columns {
		if strings.Contains(c, "body") || strings.Contains(c, "prompt") || strings.Contains(c, "content") {
			t.Fatalf("content-bearing column %q", c)
		}
	}
}
