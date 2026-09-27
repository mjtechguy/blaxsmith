package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func modelAttemptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL for PostgreSQL bootstrap test")
	}
	ctx := tenant.System(t.Context())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_model_release_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(tenant.System(context.Background()), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
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

func modelAttemptFixture(t *testing.T) (*pgxpool.Pool, *Ledger, *access.SecretStore, ModelAttempt, Runtime, Redeemed) {
	t.Helper()
	return modelAttemptFixtureWith(t, modelFixtureSpec{})
}

// modelFixtureSpec varies the fixture's model connection; the zero value is
// an organization OpenAI API key granted to a workload.
type modelFixtureSpec struct {
	provider, model, origin, authMethod, key string
	ownerKind, owner, granteeKind, grantee   string // owner "" = the organization
	baseURL                                  string // the connection's model endpoint; "" = the provider's
}

func modelAttemptFixtureWith(t *testing.T, spec modelFixtureSpec) (*pgxpool.Pool, *Ledger, *access.SecretStore, ModelAttempt, Runtime, Redeemed) {
	t.Helper()
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&spec.provider, "openai")
	def(&spec.model, "gpt-6-luna")
	def(&spec.origin, "https://api.openai.com")
	def(&spec.authMethod, "api_key")
	def(&spec.key, "private-provider-key")
	def(&spec.ownerKind, "organization")
	def(&spec.granteeKind, "workload")
	def(&spec.grantee, "worker")
	ctx := tenant.System(t.Context())
	pool := modelAttemptPool(t)
	var orgID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'model-release','Model release') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	projectID, err := store.CreateProject(ctx, orgID, "model-project", "Model project")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID,
		LaunchKey: "model-release", SourceCommit: strings.Repeat("a", 40),
		BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	taskID, err := store.AddTask(ctx, orgID, run.ID, "implement", strings.Repeat("d", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true
		WHERE organization_id=$1 AND id=$2`, orgID, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, orgID, run.ID, taskID)
	if err != nil {
		t.Fatal(err)
	}
	runtimeBinding := workflow.RuntimeBinding{AXAtespace: "space", AXTask: "task", ActorUID: "actor",
		TemplateUID: "template", Image: "registry.example/tool@sha256:" + strings.Repeat("e", 64),
		WorkerPool: "pool", CommandSHA256: strings.Repeat("f", 64)}
	if err := store.BindRuntime(ctx, attempt, runtimeBinding); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	def(&spec.owner, orgID)
	if spec.authMethod == access.ClaudeSetupTokenAuth {
		if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET allow_member_claude_subscription=true WHERE id=$1`, orgID); err != nil {
			t.Fatal(err)
		}
	}
	if spec.granteeKind == "user" {
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET initiator_principal_id=$3 WHERE organization_id=$1 AND id=$2`,
			orgID, run.ID, spec.grantee); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO access_provider_registrations
			(organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ($1,'provider',$2,$3,ARRAY['native_raw'],'active')`, []any{orgID, spec.provider, spec.origin}},
		{`INSERT INTO access_connections
			(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,base_url)
			VALUES ($1,'connection',$2,$3,'provider','account',$4,'active',$5)`, []any{orgID, spec.ownerKind, spec.owner, spec.authMethod, spec.baseURL}},
		{`INSERT INTO access_project_policies
			(organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ($1,$2,1,false,ARRAY['native_raw'])`, []any{orgID, projectID}},
		{`INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id,expires_at)
			VALUES ($1,'grant','connection',$2,$3,$4,'model.invoke',$5,'native_raw','owner',clock_timestamp()+interval '1 hour')`,
			[]any{orgID, projectID, spec.granteeKind, spec.grantee, spec.provider + "/" + spec.model}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	secrets, err := access.NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Rotate(ctx, orgID, "connection", 0, []byte(spec.key), nil); err != nil {
		t.Fatal(err)
	}
	grant := access.ModelGrant{OrganizationID: orgID, ProjectID: projectID, GrantID: "grant",
		GranteeKind: spec.granteeKind, GranteeID: spec.grantee, Provider: spec.provider, Model: spec.model}
	preflight, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := access.PreflightModelInvoke(ctx, preflight, grant)
	_ = preflight.Rollback(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bind, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bindingID, err := access.BindModelInvoke(ctx, bind, grant, approval, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := bind.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(pool)
	actor := Actor{Atespace: runtimeBinding.AXAtespace, Name: runtimeBinding.AXTask, UID: runtimeBinding.ActorUID}
	scope, err := ledger.Assign(ctx, "cluster", attempt.ID, 0, actor)
	if err != nil {
		t.Fatal(err)
	}
	offer, err := ledger.Issue(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	proof, roots := signedProof(t, offer)
	redeemed, err := ledger.Redeem(ctx, scope, offer.ID, offer.Nonce, proof, roots)
	if err != nil {
		t.Fatal(err)
	}
	model := ModelAttempt{Scope: scope, Attempt: attempt, Runtime: runtimeBinding, TTL: 10 * time.Minute,
		Invoke: access.ModelInvoke{OrganizationID: orgID, ProjectID: projectID, AttemptID: attempt.ID,
			BindingID: bindingID, GranteeKind: spec.granteeKind, GranteeID: spec.grantee,
			Provider: spec.provider, Model: spec.model, PolicyVersion: 1}}
	runtime := Runtime{Actor: actor, TemplateUID: runtimeBinding.TemplateUID,
		Image: runtimeBinding.Image, WorkerPool: runtimeBinding.WorkerPool}
	return pool, ledger, secrets, model, runtime, redeemed
}

func modelPhaseFixture(t *testing.T, pool *pgxpool.Pool, ledger *Ledger, model ModelAttempt) Redeemed {
	t.Helper()
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(tenant.System(t.Context()), model.Attempt); err != nil {
		t.Fatal(err)
	}
	offer, err := ledger.IssuePhase(tenant.System(t.Context()), model.Scope, PhaseModel)
	if err != nil {
		t.Fatal(err)
	}
	proof, roots := signedProof(t, offer)
	redeemed, err := ledger.Redeem(tenant.System(t.Context()), model.Scope, offer.ID, offer.Nonce, proof, roots)
	if err != nil {
		t.Fatal(err)
	}
	return redeemed
}

func TestModelAttemptCallbacksPostgres(t *testing.T) {
	t.Run("git setup precedes post-ready model release", func(t *testing.T) {
		pool, ledger, secrets, model, runtime, redeemed := modelAttemptFixture(t)
		const repoURL = "https://github.com/owner/repo.git"
		commit := strings.Repeat("a", 40)
		if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE access_project_policies SET git_read_enabled=true
			WHERE organization_id=$1 AND project_id=$2`, model.Invoke.OrganizationID, model.Invoke.ProjectID); err != nil {
			t.Fatal(err)
		}
		statements := []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO access_provider_registrations
				(organization_id,id,provider_kind,origin,delivery_modes,state)
				VALUES ($1,'git-provider','git','https://github.com',ARRAY['native_raw'],'active')`,
				[]any{model.Invoke.OrganizationID}},
			{`INSERT INTO access_connections
				(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
				VALUES ($1,'git-connection','organization',$1,'git-provider','account','api_key','active')`,
				[]any{model.Invoke.OrganizationID}},
			{`INSERT INTO access_grants
				(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id,expires_at)
				VALUES ($1,'git-grant','git-connection',$2,'workload','worker','git.read',$3,'native_raw','owner',clock_timestamp()+interval '1 hour')`,
				[]any{model.Invoke.OrganizationID, model.Invoke.ProjectID, repoURL}},
			{`INSERT INTO access_bindings
				(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,input_commit,policy_version)
				VALUES ($1,'git-binding',$2,$3,'git-grant',1,'git.read',$4,$5,1)`,
				[]any{model.Invoke.OrganizationID, model.Attempt.ID, model.Invoke.ProjectID, repoURL, commit}},
		}
		for _, statement := range statements {
			if _, err := pool.Exec(tenant.System(t.Context()), statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := secrets.Rotate(tenant.System(t.Context()), model.Invoke.OrganizationID, "git-connection", 0, []byte("private-git-token"), nil); err != nil {
			t.Fatal(err)
		}
		model.Git = &GitAttempt{Read: access.GitRead{OrganizationID: model.Invoke.OrganizationID,
			ProjectID: model.Invoke.ProjectID, AttemptID: model.Attempt.ID, BindingID: "git-binding",
			GranteeKind: "workload", GranteeID: "worker", RepoURL: repoURL, Commit: commit, PolicyVersion: 1},
			Username: "blaxsmith-test"}
		connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model)
		if err != nil {
			t.Fatal(err)
		}
		reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
		_, err = ledger.Release(tenant.System(t.Context()), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
			if err := connector.Authorize(ctx, tx, runtime, redeemed.Challenge); err != nil {
				return err
			}
			git, err := connector.GitSetup(ctx, tx, runtime)
			if err != nil {
				return err
			}
			defer clear(git.Token)
			if git.RepoURL != repoURL || git.Commit != commit || git.Username != "blaxsmith-test" ||
				string(git.Token) != "private-git-token" {
				return ErrDenied
			}
			if _, err := connector.ModelCredential(ctx, tx, runtime); !errors.Is(err, ErrDenied) {
				return errors.New("model credential was available during setup phase")
			}
			if err := ledger.BindActivation(ctx, tx, redeemed, runtime); err != nil {
				return err
			}
			return connector.Delivered(ctx, tx, redeemed)
		})
		if err != nil {
			t.Fatal(err)
		}
		var count, delivered int
		if err := pool.QueryRow(tenant.System(t.Context()), `SELECT count(*),count(*) FILTER (WHERE delivered_at IS NOT NULL)
			FROM access_leases WHERE bootstrap_challenge_id=$1`, redeemed.ID).Scan(&count, &delivered); err != nil || count != 1 || delivered != 1 {
			t.Fatalf("setup capability leases: count=%d delivered=%d err=%v", count, delivered, err)
		}
		modelRedeemed := modelPhaseFixture(t, pool, ledger, model)
		reserveModel := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, modelRedeemed) }
		_, err = ledger.Release(tenant.System(t.Context()), modelRedeemed, reserveModel, func(ctx context.Context, tx pgx.Tx) error {
			if err := connector.Authorize(ctx, tx, runtime, modelRedeemed.Challenge); err != nil {
				return err
			}
			credential, err := connector.ModelCredential(ctx, tx, runtime)
			if err != nil {
				return err
			}
			defer clear(credential.APIKey)
			if string(credential.APIKey) != "private-provider-key" {
				return ErrDenied
			}
			if err := ledger.BindActivation(ctx, tx, modelRedeemed, runtime); err != nil {
				return err
			}
			return connector.Delivered(ctx, tx, modelRedeemed)
		})
		if err != nil {
			t.Fatal(err)
		}
		var phase string
		if err := pool.QueryRow(tenant.System(t.Context()), `SELECT c.phase FROM access_leases l JOIN bootstrap_challenges c
			ON c.id=l.bootstrap_challenge_id WHERE l.attempt_id=$1 AND l.capability='model.invoke'`,
			model.Attempt.ID).Scan(&phase); err != nil || phase != PhaseModel {
			t.Fatalf("model lease was not bound to model phase: %q %v", phase, err)
		}
	})

	t.Run("same attempt release", func(t *testing.T) {
		pool, ledger, secrets, model, runtime, redeemed := modelAttemptFixture(t)
		redeemed = modelPhaseFixture(t, pool, ledger, model)
		connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model)
		if err != nil {
			t.Fatal(err)
		}
		reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
		_, err = ledger.Release(tenant.System(t.Context()), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
			if err := connector.Authorize(ctx, tx, runtime, redeemed.Challenge); err != nil {
				return err
			}
			credential, err := connector.ModelCredential(ctx, tx, runtime)
			if err != nil {
				return err
			}
			defer clear(credential.APIKey)
			if credential.AttemptID != model.Attempt.ID || credential.Provider != "openai" ||
				string(credential.APIKey) != "private-provider-key" {
				return ErrDenied
			}
			if err := ledger.BindActivation(ctx, tx, redeemed, runtime); err != nil {
				return err
			}
			return connector.Delivered(ctx, tx, redeemed)
		})
		if err != nil {
			t.Fatal(err)
		}
		var delivered bool
		if err := pool.QueryRow(tenant.System(t.Context()), `SELECT delivered_at IS NOT NULL FROM access_leases
			WHERE organization_id=$1 AND binding_id=$2 AND attempt_id=$3`, model.Invoke.OrganizationID,
			model.Invoke.BindingID, model.Attempt.ID).Scan(&delivered); err != nil || !delivered {
			t.Fatalf("same-attempt lease not delivered: %t %v", delivered, err)
		}
		// A still-authorized running attempt's short lease is renewed near expiry.
		renewed, err := access.RenewModelLeases(tenant.System(t.Context()), pool, 30*time.Minute, "", "", 100)
		if err != nil || len(renewed) != 1 || renewed[0].AttemptID != model.Attempt.ID ||
			time.Until(renewed[0].ExpiresAt) < 25*time.Minute {
			t.Fatalf("renewal: %+v %v", renewed, err)
		}
		ctx := tenant.System(t.Context())
		// Losing delivery (or its acknowledgement) replays the exact renewal.
		for range 2 {
			if again, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 100); err != nil || len(again) != 1 || again[0] != renewed[0] {
				t.Fatalf("pending renewal was not replayed: %+v %v", again, err)
			}
		}
		wrongOrg := renewed[0]
		wrongOrg.OrganizationID = "another-org"
		if err := access.MarkRenewalDelivered(ctx, pool, wrongOrg); !errors.Is(err, access.ErrDenied) {
			t.Fatalf("acknowledgement crossed organizations: %v", err)
		}
		for range 2 { // acknowledging twice is harmless
			if err := access.MarkRenewalDelivered(ctx, pool, renewed[0]); err != nil {
				t.Fatal(err)
			}
		}
		if again, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 100); err != nil || len(again) != 0 {
			t.Fatalf("a fresh lease was renewed again: %+v %v", again, err)
		}
		testRenewalPages(t, pool, renewed[0])
		// A stale acknowledgement cannot consume the next renewal.
		if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE access_leases SET expires_at=clock_timestamp()+interval '1 minute'
			WHERE attempt_id=$1`, model.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		next, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 100)
		if err != nil || len(next) != 1 || next[0].Generation != renewed[0].Generation+1 {
			t.Fatalf("next renewal: %+v %v", next, err)
		}
		if err := access.MarkRenewalDelivered(ctx, pool, renewed[0]); !errors.Is(err, access.ErrDenied) {
			t.Fatalf("stale acknowledgement accepted: %v", err)
		}
		if again, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 100); err != nil || len(again) != 1 || again[0] != next[0] {
			t.Fatalf("stale acknowledgement consumed renewal: %+v %v", again, err)
		}
		// Cancellation fences even an already-pending renewal.
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET state='cancel_requested' WHERE id=$1`, model.Attempt.RunID); err != nil {
			t.Fatal(err)
		}
		if again, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 100); err != nil || len(again) != 0 {
			t.Fatalf("cancelled run renewal delivered: %+v %v", again, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET state='active' WHERE id=$1`, model.Attempt.RunID); err != nil {
			t.Fatal(err)
		}
		// A revoked grant never replays pending delivery.
		var grantID string
		if err := pool.QueryRow(tenant.System(t.Context()), `SELECT grant_id FROM access_bindings WHERE organization_id=$1 AND id=$2`,
			model.Invoke.OrganizationID, model.Invoke.BindingID).Scan(&grantID); err != nil {
			t.Fatal(err)
		}
		if err := access.RevokeGrant(tenant.System(t.Context()), pool, model.Invoke.OrganizationID, grantID); err != nil {
			t.Fatal(err)
		}
		if revoked, err := access.RenewModelLeases(tenant.System(t.Context()), pool, 30*time.Minute, "", "", 100); err != nil || len(revoked) != 0 {
			t.Fatalf("revoked lease renewed: %+v %v", revoked, err)
		}
		if err := access.MarkRenewalDelivered(ctx, pool, next[0]); !errors.Is(err, access.ErrDenied) {
			t.Fatalf("revoked lease acknowledgement accepted: %v", err)
		}
	})
	t.Run("grant revoked after intent", func(t *testing.T) {
		pool, ledger, secrets, model, runtime, redeemed := modelAttemptFixture(t)
		redeemed = modelPhaseFixture(t, pool, ledger, model)
		connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model)
		if err != nil {
			t.Fatal(err)
		}
		reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
		_, err = ledger.Release(tenant.System(t.Context()), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
			if err := access.RevokeGrant(ctx, pool, model.Invoke.OrganizationID, "grant"); err != nil {
				return err
			}
			return connector.Authorize(ctx, tx, runtime, redeemed.Challenge)
		})
		if !errors.Is(err, access.ErrDenied) {
			t.Fatalf("revocation before release was not fenced: %v", err)
		}
		var attempted, delivered, revoked bool
		if err := pool.QueryRow(tenant.System(t.Context()), `SELECT delivery_attempted_at IS NOT NULL,delivered_at IS NOT NULL,revoked_at IS NOT NULL
			FROM access_leases WHERE organization_id=$1 AND binding_id=$2`, model.Invoke.OrganizationID,
			model.Invoke.BindingID).Scan(&attempted, &delivered, &revoked); err != nil || attempted || delivered || !revoked {
			t.Fatalf("revoked intent incorrectly delivered: %t %t %t %v", attempted, delivered, revoked, err)
		}
	})
	t.Run("binding belongs to another attempt", func(t *testing.T) {
		pool, ledger, secrets, model, _, redeemed := modelAttemptFixture(t)
		redeemed = modelPhaseFixture(t, pool, ledger, model)
		if _, err := pool.Exec(tenant.System(t.Context()), `INSERT INTO access_bindings
			(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,policy_version)
			VALUES ($1,'other-binding','other-attempt',$2,'grant',1,'model.invoke','openai/gpt-6-luna',1)`,
			model.Invoke.OrganizationID, model.Invoke.ProjectID); err != nil {
			t.Fatal(err)
		}
		model.Invoke.BindingID = "other-binding"
		connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model)
		if err != nil {
			t.Fatal(err)
		}
		reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
		if _, err := ledger.Release(tenant.System(t.Context()), redeemed, reserve,
			func(context.Context, pgx.Tx) error { return nil }); !errors.Is(err, access.ErrDenied) {
			t.Fatalf("foreign attempt binding reached release: %v", err)
		}
	})
}

func testRenewalPages(t *testing.T, pool *pgxpool.Pool, seed access.RenewedLease) {
	t.Helper()
	ctx := tenant.System(t.Context())
	// Clone three delivered leases in reverse order; the acknowledged seed
	// remains fresh and is not eligible for this pass.
	for _, id := range []string{"page-3", "page-2", "page-1"} {
		if _, err := pool.Exec(ctx, `INSERT INTO bootstrap_challenges
			(id,cluster_id,attempt_id,owner_generation,actor_atespace,actor_name,actor_uid,nonce_sha256,expires_at,consumed_at,release_attempted_at,phase)
			SELECT $3,c.cluster_id,c.attempt_id,c.owner_generation,c.actor_atespace,c.actor_name,c.actor_uid,
			decode(md5($3)||md5($3),'hex'),c.expires_at,c.consumed_at,c.release_attempted_at,c.phase
			FROM bootstrap_challenges c JOIN access_leases l ON l.bootstrap_challenge_id=c.id
			WHERE l.organization_id=$1 AND l.id=$2`, seed.OrganizationID, seed.LeaseID, id); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO access_leases
			(organization_id,id,binding_id,connection_id,bootstrap_challenge_id,cluster_id,attempt_id,
			owner_generation,actor_uid,capability,resource,audience,secret_version,expires_at,delivery_attempted_at,delivered_at)
			SELECT organization_id,$3,binding_id,connection_id,$3,cluster_id,attempt_id,
			owner_generation,actor_uid,capability,resource,audience,secret_version,clock_timestamp()+interval '1 minute',delivery_attempted_at,delivered_at
			FROM access_leases WHERE organization_id=$1 AND id=$2`, seed.OrganizationID, seed.LeaseID, id); err != nil {
			t.Fatal(err)
		}
	}
	first, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 1)
	if err != nil || len(first) != 1 || first[0].LeaseID != "page-1" {
		t.Fatalf("first renewal page: %+v %v", first, err)
	}
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_leases WHERE renewal_pending`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("renewal extended leases beyond the page: pending=%d err=%v", pending, err)
	}
	// No acknowledgement of page one: its failure must not block page two.
	next, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, first[0].OrganizationID, first[0].LeaseID, 2)
	if err != nil || len(next) != 2 || next[0].LeaseID != "page-2" || next[1].LeaseID != "page-3" {
		t.Fatalf("next renewal page: %+v %v", next, err)
	}
	if end, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, next[1].OrganizationID, next[1].LeaseID, 2); err != nil || len(end) != 0 {
		t.Fatalf("renewal pass did not end: %+v %v", end, err)
	}
	if retry, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", 1); err != nil || len(retry) != 1 || retry[0] != first[0] {
		t.Fatalf("wrapped pass did not replay pending renewal: %+v %v", retry, err)
	}
	for _, limit := range []int{0, 101} {
		if _, err := access.RenewModelLeases(ctx, pool, 30*time.Minute, "", "", limit); !errors.Is(err, access.ErrDenied) {
			t.Fatalf("invalid renewal page limit %d accepted: %v", limit, err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM access_leases WHERE organization_id=$1 AND id IN ('page-1','page-2','page-3')`, seed.OrganizationID); err != nil {
		t.Fatal(err)
	}
}
