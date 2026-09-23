package access

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
)

func TestGitReadAuthorityPostgres(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	const repo = "https://git.example.invalid/team/private.git"
	const commit = "0123456789abcdef0123456789abcdef01234567"
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ('org-a','git-test','git','https://git.example.invalid',ARRAY['native_raw'],'active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ('org-a','connection-a','organization','org-a','git-test','account-a','test','active')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_project_policies
		(organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ('org-a','project-a',1,true,ARRAY['native_raw'])`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id,expires_at)
		VALUES ('org-a','grant-a','connection-a','project-a','user','alice','git.read',$1,'native_raw','owner',clock_timestamp()+interval '1 hour')`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_bindings
		(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,input_commit,policy_version)
		VALUES ('org-a','binding-a','attempt-a','project-a','grant-a',1,'git.read',$1,$2,1)`, repo, commit); err != nil {
		t.Fatal(err)
	}
	request := GitRead{OrganizationID: "org-a", ProjectID: "project-a", AttemptID: "attempt-a",
		BindingID: "binding-a", GranteeKind: "user", GranteeID: "alice", RepoURL: repo,
		Commit: commit, PolicyVersion: 1}
	check := func(r GitRead) (Decision, error) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return AuthorizeGitRead(ctx, tx, r)
	}
	decision, err := check(request)
	if err != nil || decision.ConnectionID != "connection-a" || decision.ProviderID != "git-test" || decision.ExternalAccountID != "account-a" ||
		decision.GrantID != "grant-a" || decision.DeliveryMode != "native_raw" {
		t.Fatalf("current Git-read authority: %+v, %v", decision, err)
	}
	for name, change := range map[string]func(*GitRead){
		"organization": func(r *GitRead) { r.OrganizationID = "org-b" },
		"project":      func(r *GitRead) { r.ProjectID = "project-b" },
		"attempt":      func(r *GitRead) { r.AttemptID = "attempt-b" },
		"grantee":      func(r *GitRead) { r.GranteeID = "bob" },
		"repository":   func(r *GitRead) { r.RepoURL = "https://git.example.invalid/other.git" },
		"commit":       func(r *GitRead) { r.Commit = "0000000000000000000000000000000000000000" },
		"policy":       func(r *GitRead) { r.PolicyVersion = 2 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := request
			change(&candidate)
			if _, err := check(candidate); !errors.Is(err, ErrDenied) {
				t.Fatalf("changed scope accepted: %v", err)
			}
		})
	}
	for _, pair := range [][2]string{
		{`UPDATE access_project_policies SET version=2 WHERE organization_id='org-a' AND project_id='project-a'`,
			`UPDATE access_project_policies SET version=1 WHERE organization_id='org-a' AND project_id='project-a'`},
		{`UPDATE access_project_policies SET git_read_enabled=false WHERE organization_id='org-a' AND project_id='project-a'`,
			`UPDATE access_project_policies SET git_read_enabled=true WHERE organization_id='org-a' AND project_id='project-a'`},
		{`UPDATE access_project_policies SET delivery_modes=ARRAY['brokered'] WHERE organization_id='org-a' AND project_id='project-a'`,
			`UPDATE access_project_policies SET delivery_modes=ARRAY['native_raw'] WHERE organization_id='org-a' AND project_id='project-a'`},
	} {
		if _, err := pool.Exec(ctx, pair[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := check(request); !errors.Is(err, ErrDenied) {
			t.Fatalf("project policy change accepted after %q: %v", pair[0], err)
		}
		if _, err := pool.Exec(ctx, pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]string{
		{`UPDATE access_provider_registrations SET state='disabled' WHERE organization_id='org-a' AND id='git-test'`,
			`UPDATE access_provider_registrations SET state='active' WHERE organization_id='org-a' AND id='git-test'`},
		{`UPDATE access_provider_registrations SET origin='https://other.invalid' WHERE organization_id='org-a' AND id='git-test'`,
			`UPDATE access_provider_registrations SET origin='https://git.example.invalid' WHERE organization_id='org-a' AND id='git-test'`},
		{`UPDATE access_provider_registrations SET delivery_modes=ARRAY['brokered'] WHERE organization_id='org-a' AND id='git-test'`,
			`UPDATE access_provider_registrations SET delivery_modes=ARRAY['native_raw'] WHERE organization_id='org-a' AND id='git-test'`},
	} {
		if _, err := pool.Exec(ctx, pair[0]); err != nil {
			t.Fatal(err)
		}
		if _, err := check(request); !errors.Is(err, ErrDenied) {
			t.Fatalf("provider policy change accepted after %q: %v", pair[0], err)
		}
		if _, err := pool.Exec(ctx, pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	for _, update := range []string{
		`UPDATE access_grants SET revoked_at=clock_timestamp() WHERE organization_id='org-a' AND id='grant-a'`,
		`UPDATE access_grants SET revoked_at=NULL, version=2 WHERE organization_id='org-a' AND id='grant-a'`,
		`UPDATE access_grants SET version=1, expires_at=clock_timestamp()-interval '1 second' WHERE organization_id='org-a' AND id='grant-a'`,
		`UPDATE access_grants SET expires_at=NULL WHERE organization_id='org-a' AND id='grant-a';
		 UPDATE access_connections SET state='disabled' WHERE organization_id='org-a' AND id='connection-a'`,
	} {
		if _, err := pool.Exec(ctx, update); err != nil {
			t.Fatal(err)
		}
		if _, err := check(request); !errors.Is(err, ErrDenied) {
			t.Fatalf("changed authority accepted after %q: %v", update, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_bindings
		(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,input_commit,policy_version)
		VALUES ('org-b','cross-org','attempt-a','project-a','grant-a',1,'git.read',$1,$2,1)`, repo, commit); err == nil {
		t.Fatal("cross-organization grant reference accepted")
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL for the PostgreSQL authority test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_access_" + hex.EncodeToString(suffix[:])
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
