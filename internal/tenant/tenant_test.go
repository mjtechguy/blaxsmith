package tenant_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// scoped are representative organization-owned tables across identity, access,
// workflow, and gateway, with both uuid and text organization_id columns.
var scoped = []string{"identity_memberships", "identity_audit_events", "access_project_policies",
	"workflow_projects", "gateway_org_settings", "access_organization_keys",
	"gateway_budget_settings"}

func TestRowLevelSecurityPostgres(t *testing.T) {
	pool := rlsPool(t)
	system := tenant.System(t.Context())
	orgs := [2]string{}
	for i, slug := range []string{"rls-a", "rls-b"} {
		if err := pool.QueryRow(system, `INSERT INTO identity_organizations (id,slug,name)
			VALUES (gen_random_uuid(),$1,$1) RETURNING id`, slug).Scan(&orgs[i]); err != nil {
			t.Fatal(err)
		}
	}
	a, b := orgs[0], orgs[1]
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range orgs {
		var principal string
		if err := pool.QueryRow(system, `INSERT INTO identity_principals (id,username)
			VALUES (gen_random_uuid(),'p-'||left($1,8)) RETURNING id`, org).Scan(&principal); err != nil {
			t.Fatal(err)
		}
		for _, statement := range []string{
			`INSERT INTO identity_memberships (organization_id,principal_id,role) VALUES ($1,$2,'owner')`,
			`INSERT INTO identity_audit_events (organization_id,actor_kind,action,subject_id) VALUES ($1,'system','rls.probe',$2)`,
			`INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
				VALUES ($1,$2,1,false,'{brokered}')`,
			`INSERT INTO gateway_org_settings (organization_id,updated_by) VALUES ($1,$2)`,
			`INSERT INTO gateway_budget_settings (organization_id,updated_by) VALUES ($1,$2)`,
			`INSERT INTO access_organization_keys (organization_id,version,master_key_id,algorithm,nonce,wrapped_key)
				VALUES ($1,1,left($2,8),'AES-256-GCM',decode(repeat('00',12),'hex'),decode(repeat('00',48),'hex'))`,
		} {
			if _, err := pool.Exec(system, statement, org, principal); err != nil {
				t.Fatalf("seed %s: %v", org, err)
			}
		}
		if _, err := store.CreateProject(system, org, "rls-project", "RLS project"); err != nil {
			t.Fatal(err)
		}
	}

	count := func(ctx context.Context, table string) int {
		t.Helper()
		var n int
		// No organization filter: row-level security is the only thing scoping it.
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	onlyA := tenant.Org(t.Context(), a)
	for _, table := range scoped {
		if got := count(system, table); got != 2 {
			t.Fatalf("system %s sees %d rows, want both organizations", table, got)
		}
		if got := count(onlyA, table); got != 1 {
			t.Fatalf("organization A %s sees %d rows, want only its own", table, got)
		}
		if got := count(t.Context(), table); got != 0 {
			t.Fatalf("unscoped %s sees %d rows, want none", table, got)
		}
		var foreign int
		if err := pool.QueryRow(onlyA, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()+
			` WHERE organization_id::text=$1`, b).Scan(&foreign); err != nil || foreign != 0 {
			t.Fatalf("organization A read B's %s: %d, %v", table, foreign, err)
		}
		tag, err := pool.Exec(onlyA, `DELETE FROM `+pgx.Identifier{table}.Sanitize()+` WHERE organization_id::text=$1`, b)
		if err != nil || tag.RowsAffected() != 0 {
			t.Fatalf("organization A deleted B's %s: %v, %v", table, tag, err)
		}
	}

	// Writes into another organization, or with no scope, are rejected.
	for name, ctx := range map[string]context.Context{"other organization": onlyA, "unscoped": t.Context()} {
		_, err := pool.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,action)
			VALUES ($1,'system','rls.cross')`, b)
		if !rlsViolation(err) {
			t.Fatalf("%s insert: %v", name, err)
		}
		_, err = pool.Exec(ctx, `INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ($1,'q',1,false,'{brokered}')`, b)
		if !rlsViolation(err) {
			t.Fatalf("%s text-keyed insert: %v", name, err)
		}
	}
	if tag, err := pool.Exec(onlyA, `UPDATE workflow_projects SET organization_id=$1`, b); !rlsViolation(err) {
		t.Fatalf("moving a row into another organization: %v, %v", tag, err)
	}

	// A transaction keeps the scope of the context that began it.
	tx, err := pool.Begin(onlyA)
	if err != nil {
		t.Fatal(err)
	}
	var inTx int
	if err := tx.QueryRow(t.Context(), `SELECT count(*) FROM workflow_projects`).Scan(&inTx); err != nil || inTx != 1 {
		t.Fatalf("transaction scope: %d, %v", inTx, err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Store methods pin their own organization, whatever the caller's scope.
	for _, ctx := range []context.Context{t.Context(), tenant.Org(t.Context(), b), system} {
		projects, err := store.ListProjects(ctx, a, workflow.ListPage{Sort: "name", Direction: "asc", Limit: 10})
		if err != nil || len(projects) != 1 || projects[0].OrganizationID != a {
			t.Fatalf("store scope: %+v, %v", projects, err)
		}
	}

	// A connection reused after a scoped caller carries nothing over.
	single := rlsPoolConfig(t, pool)
	single.MaxConns = 1
	one, err := pgxpool.NewWithConfig(t.Context(), tenant.Configure(single))
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	for _, step := range []struct {
		ctx  context.Context
		want int
	}{{system, 2}, {t.Context(), 0}, {onlyA, 1}, {t.Context(), 0}} {
		var n int
		if err := one.QueryRow(step.ctx, `SELECT count(*) FROM workflow_projects`).Scan(&n); err != nil || n != step.want {
			t.Fatalf("reused connection sees %d, want %d: %v", n, step.want, err)
		}
	}
}

func rlsViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "42501"
}

func rlsPoolConfig(t *testing.T, pool *pgxpool.Pool) *pgxpool.Config {
	t.Helper()
	config, err := pgxpool.ParseConfig(pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = pool.Config().ConnConfig.RuntimeParams["search_path"]
	return config
}

func rlsPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var bypass bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).Scan(&bypass); err != nil {
		t.Fatal(err)
	}
	if bypass {
		t.Skip("BLAXSMITH_TEST_DATABASE_URL must name a role without SUPERUSER or BYPASSRLS; row-level security does not apply to it")
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_tenant_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, tenant.Configure(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
