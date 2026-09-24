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
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func modelAttemptPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL for PostgreSQL bootstrap test")
	}
	ctx := t.Context()
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

func modelAttemptFixture(t *testing.T) (*pgxpool.Pool, *Ledger, *access.SecretStore, ModelAttempt, Runtime, Redeemed) {
	t.Helper()
	ctx := t.Context()
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
	for _, statement := range []string{
		`INSERT INTO access_provider_registrations
			(organization_id,id,provider_kind,origin,delivery_modes,state)
			VALUES ($1,'provider','openai','https://api.openai.com',ARRAY['native_raw'],'active')`,
		`INSERT INTO access_connections
			(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
			VALUES ($1,'connection','organization',$1,'provider','account','api_key','active')`,
		`INSERT INTO access_project_policies
			(organization_id,project_id,version,git_read_enabled,delivery_modes)
			VALUES ($1,$2,1,false,ARRAY['native_raw'])`,
		`INSERT INTO access_grants
			(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id,expires_at)
			VALUES ($1,'grant','connection',$2,'workload','worker','model.invoke','openai/gpt-6-luna','native_raw','owner',clock_timestamp()+interval '1 hour')`,
	} {
		args := []any{orgID}
		if strings.Contains(statement, "$2") {
			args = append(args, projectID)
		}
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	secrets, err := access.NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secrets.Rotate(ctx, orgID, "connection", 0, []byte("private-provider-key"), nil); err != nil {
		t.Fatal(err)
	}
	grant := access.ModelGrant{OrganizationID: orgID, ProjectID: projectID, GrantID: "grant",
		GranteeKind: "workload", GranteeID: "worker", Provider: "openai", Model: "gpt-6-luna"}
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
			BindingID: bindingID, GranteeKind: "workload", GranteeID: "worker",
			Provider: "openai", Model: "gpt-6-luna", PolicyVersion: 1}}
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
	if err := store.ConfirmStarted(t.Context(), model.Attempt); err != nil {
		t.Fatal(err)
	}
	offer, err := ledger.IssuePhase(t.Context(), model.Scope, PhaseModel)
	if err != nil {
		t.Fatal(err)
	}
	proof, roots := signedProof(t, offer)
	redeemed, err := ledger.Redeem(t.Context(), model.Scope, offer.ID, offer.Nonce, proof, roots)
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
		if _, err := pool.Exec(t.Context(), `UPDATE access_project_policies SET git_read_enabled=true
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
			if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := secrets.Rotate(t.Context(), model.Invoke.OrganizationID, "git-connection", 0, []byte("private-git-token"), nil); err != nil {
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
		_, err = ledger.Release(t.Context(), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
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
		if err := pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER (WHERE delivered_at IS NOT NULL)
			FROM access_leases WHERE bootstrap_challenge_id=$1`, redeemed.ID).Scan(&count, &delivered); err != nil || count != 1 || delivered != 1 {
			t.Fatalf("setup capability leases: count=%d delivered=%d err=%v", count, delivered, err)
		}
		modelRedeemed := modelPhaseFixture(t, pool, ledger, model)
		reserveModel := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, modelRedeemed) }
		_, err = ledger.Release(t.Context(), modelRedeemed, reserveModel, func(ctx context.Context, tx pgx.Tx) error {
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
		if err := pool.QueryRow(t.Context(), `SELECT c.phase FROM access_leases l JOIN bootstrap_challenges c
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
		_, err = ledger.Release(t.Context(), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
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
		if err := pool.QueryRow(t.Context(), `SELECT delivered_at IS NOT NULL FROM access_leases
			WHERE organization_id=$1 AND binding_id=$2 AND attempt_id=$3`, model.Invoke.OrganizationID,
			model.Invoke.BindingID, model.Attempt.ID).Scan(&delivered); err != nil || !delivered {
			t.Fatalf("same-attempt lease not delivered: %t %v", delivered, err)
		}
		// A still-authorized running attempt's short lease is renewed near expiry.
		renewed, err := access.RenewModelLeases(t.Context(), pool, 30*time.Minute)
		if err != nil || len(renewed) != 1 || renewed[0].AttemptID != model.Attempt.ID ||
			time.Until(renewed[0].ExpiresAt) < 25*time.Minute {
			t.Fatalf("renewal: %+v %v", renewed, err)
		}
		if again, err := access.RenewModelLeases(t.Context(), pool, 30*time.Minute); err != nil || len(again) != 0 {
			t.Fatalf("a fresh lease was renewed again: %+v %v", again, err)
		}
		// A revoked grant is never renewed; the worker stops at the old expiry.
		if _, err := pool.Exec(t.Context(), `UPDATE access_leases SET expires_at=clock_timestamp()+interval '1 minute'
			WHERE attempt_id=$1`, model.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		var grantID string
		if err := pool.QueryRow(t.Context(), `SELECT grant_id FROM access_bindings WHERE organization_id=$1 AND id=$2`,
			model.Invoke.OrganizationID, model.Invoke.BindingID).Scan(&grantID); err != nil {
			t.Fatal(err)
		}
		if err := access.RevokeGrant(t.Context(), pool, model.Invoke.OrganizationID, grantID); err != nil {
			t.Fatal(err)
		}
		if revoked, err := access.RenewModelLeases(t.Context(), pool, 30*time.Minute); err != nil || len(revoked) != 0 {
			t.Fatalf("revoked lease renewed: %+v %v", revoked, err)
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
		_, err = ledger.Release(t.Context(), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
			if err := access.RevokeGrant(ctx, pool, model.Invoke.OrganizationID, "grant"); err != nil {
				return err
			}
			return connector.Authorize(ctx, tx, runtime, redeemed.Challenge)
		})
		if !errors.Is(err, access.ErrDenied) {
			t.Fatalf("revocation before release was not fenced: %v", err)
		}
		var attempted, delivered, revoked bool
		if err := pool.QueryRow(t.Context(), `SELECT delivery_attempted_at IS NOT NULL,delivered_at IS NOT NULL,revoked_at IS NOT NULL
			FROM access_leases WHERE organization_id=$1 AND binding_id=$2`, model.Invoke.OrganizationID,
			model.Invoke.BindingID).Scan(&attempted, &delivered, &revoked); err != nil || attempted || delivered || !revoked {
			t.Fatalf("revoked intent incorrectly delivered: %t %t %t %v", attempted, delivered, revoked, err)
		}
	})
	t.Run("binding belongs to another attempt", func(t *testing.T) {
		pool, ledger, secrets, model, _, redeemed := modelAttemptFixture(t)
		redeemed = modelPhaseFixture(t, pool, ledger, model)
		if _, err := pool.Exec(t.Context(), `INSERT INTO access_bindings
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
		if _, err := ledger.Release(t.Context(), redeemed, reserve,
			func(context.Context, pgx.Tx) error { return nil }); !errors.Is(err, access.ErrDenied) {
			t.Fatalf("foreign attempt binding reached release: %v", err)
		}
	})
}
