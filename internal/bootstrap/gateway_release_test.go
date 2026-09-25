package bootstrap

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/gateway"
)

// gatewayRelease runs the model-phase release for an attempt dispatched in
// brokered_gateway mode and returns the credential the sandbox received.
func gatewayRelease(t *testing.T, enabled bool) (*pgxpool.Pool, *access.SecretStore, ModelAttempt, string, error) {
	t.Helper()
	return gatewayReleaseWith(t, enabled, modelFixtureSpec{}, "codex")
}

func gatewayReleaseWith(t *testing.T, enabled bool, spec modelFixtureSpec, harness string) (*pgxpool.Pool, *access.SecretStore, ModelAttempt, string, error) {
	t.Helper()
	pool, ledger, secrets, model, runtime, _ := modelAttemptFixtureWith(t, spec)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_org_settings (organization_id,enabled) VALUES ($1,$2)`,
		model.Invoke.OrganizationID, enabled); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_attempt_delivery (organization_id,attempt_id,delivery_mode,base_url,harness)
		VALUES ($1,$2,'brokered_gateway','https://gw.example',$3)`, model.Invoke.OrganizationID, model.Attempt.ID, harness); err != nil {
		t.Fatal(err)
	}
	model.Gateway = func(ctx context.Context, tx pgx.Tx, leaseID string) ([]byte, bool, error) {
		return gateway.Mint(ctx, tx, gateway.MintRequest{Invoke: model.Invoke, RunID: model.Attempt.RunID,
			TaskID: model.Attempt.TaskID, LeaseID: leaseID, OwnerGeneration: model.Scope.OwnerGeneration})
	}
	redeemed := modelPhaseFixture(t, pool, ledger, model)
	connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model)
	if err != nil {
		t.Fatal(err)
	}
	var token string
	reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
	_, err = ledger.Release(ctx, redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
		if err := connector.Authorize(ctx, tx, runtime, redeemed.Challenge); err != nil {
			return err
		}
		credential, err := connector.ModelCredential(ctx, tx, runtime)
		if err != nil {
			return err
		}
		token = string(credential.APIKey)
		if err := ledger.BindActivation(ctx, tx, redeemed, runtime); err != nil {
			return err
		}
		return connector.Delivered(ctx, tx, redeemed)
	})
	return pool, secrets, model, token, err
}

func TestGatewayTokenLifecyclePostgres(t *testing.T) {
	authorize := func(t *testing.T, pool *pgxpool.Pool, secrets *access.SecretStore, token string) (gateway.Grant, error) {
		t.Helper()
		a := &gateway.Authorizer{DB: pool, Secrets: secrets}
		grant, err := a.Authorize(t.Context(), token, "openai")
		clear(grant.Key)
		return grant, err
	}
	t.Run("mint releases a token, never the key, and authorizes", func(t *testing.T) {
		pool, secrets, model, token, err := gatewayRelease(t, true)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(token, gateway.TokenPrefix) || strings.Contains(token, "private-provider-key") {
			t.Fatalf("sandbox received %q, want a gateway token", token)
		}
		var stored int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gateway_tokens WHERE token_sha256=$1`,
			func() []byte { h := gateway.HashToken(token); return h[:] }()).Scan(&stored); err != nil || stored != 1 {
			t.Fatalf("token hash stored: %d %v", stored, err)
		}
		var plain int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM gateway_tokens WHERE position($1 in encode(token_sha256,'escape'))>0`,
			token).Scan(&plain); err != nil || plain != 0 {
			t.Fatalf("plaintext token stored: %d %v", plain, err)
		}
		a := &gateway.Authorizer{DB: pool, Secrets: secrets}
		grant, err := a.Authorize(t.Context(), token, "openai")
		if err != nil || string(grant.Key) != "private-provider-key" || grant.AttemptID != model.Attempt.ID ||
			grant.Model != "gpt-6-luna" || grant.StageKey != "implement" || grant.Harness != "codex" {
			t.Fatalf("authorize: %+v %v", grant, err)
		}
		clear(grant.Key)
		if _, err := authorize(t, pool, secrets, token+"x"); !errors.Is(err, gateway.ErrDenied) {
			t.Fatalf("altered token accepted: %v", err)
		}
		if _, err := (&gateway.Authorizer{DB: pool, Secrets: secrets}).Authorize(t.Context(), token, "anthropic"); !errors.Is(err, gateway.ErrDenied) {
			t.Fatalf("token used for another family: %v", err)
		}
	})
	t.Run("renewed with the lease", func(t *testing.T) {
		pool, secrets, model, token, err := gatewayRelease(t, true)
		if err != nil {
			t.Fatal(err)
		}
		// Near expiry the existing renewal loop extends the lease; the token's
		// expiry is the lease's, so it follows with no extra hook.
		if _, err := pool.Exec(t.Context(), `UPDATE access_leases SET expires_at=clock_timestamp()+interval '2 minutes'
			WHERE attempt_id=$1`, model.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		renewed, err := access.RenewModelLeases(t.Context(), pool, 30*time.Minute)
		if err != nil || len(renewed) != 1 {
			t.Fatalf("renewal: %+v %v", renewed, err)
		}
		grant, err := authorize(t, pool, secrets, token)
		if err != nil || time.Until(grant.ExpiresAt) < 25*time.Minute {
			t.Fatalf("token did not follow the renewed lease: %v %v", grant.ExpiresAt, err)
		}
		// An expired lease denies the next request.
		if _, err := pool.Exec(t.Context(), `UPDATE access_leases SET expires_at=clock_timestamp()-interval '1 second'
			WHERE attempt_id=$1`, model.Attempt.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := authorize(t, pool, secrets, token); !errors.Is(err, gateway.ErrDenied) {
			t.Fatalf("expired lease authorized: %v", err)
		}
	})
	t.Run("revocation denies the next request", func(t *testing.T) {
		for name, revoke := range map[string]string{
			"grant":   `UPDATE access_grants SET revoked_at=clock_timestamp(),version=version+1 WHERE organization_id=$1`,
			"lease":   `UPDATE access_leases SET revoked_at=clock_timestamp() WHERE organization_id=$1`,
			"token":   `UPDATE gateway_tokens SET revoked_at=clock_timestamp() WHERE organization_id=$1::uuid`,
			"attempt": `UPDATE workflow_attempts SET state='stopped',finished_at=clock_timestamp() WHERE organization_id=$1::uuid`,
		} {
			t.Run(name, func(t *testing.T) {
				pool, secrets, model, token, err := gatewayRelease(t, true)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := authorize(t, pool, secrets, token); err != nil {
					t.Fatalf("before revocation: %v", err)
				}
				if name == "attempt" { // the task row must release its active attempt first.
					if _, err := pool.Exec(t.Context(), `UPDATE workflow_tasks SET state='blocked',active_attempt_id=NULL
						WHERE organization_id=$1::uuid`, model.Invoke.OrganizationID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := pool.Exec(t.Context(), revoke, model.Invoke.OrganizationID); err != nil {
					t.Fatal(err)
				}
				if _, err := authorize(t, pool, secrets, token); !errors.Is(err, gateway.ErrDenied) {
					t.Fatalf("%s revocation did not deny: %v", name, err)
				}
			})
		}
	})
	t.Run("authorization parity with AuthorizeModelInvoke", func(t *testing.T) {
		// Each mutation makes AuthorizeModelInvoke deny; the gateway, which
		// calls it on every request, must deny too.
		for name, mutate := range map[string]string{
			"policy version":     `UPDATE access_project_policies SET version=version+1 WHERE organization_id=$1`,
			"policy mode":        `UPDATE access_project_policies SET delivery_modes=ARRAY['oauth_access'] WHERE organization_id=$1`,
			"connection revoked": `UPDATE access_connections SET state='revoked' WHERE organization_id=$1`,
			"provider disabled":  `UPDATE access_provider_registrations SET state='disabled' WHERE organization_id=$1`,
			"grant expired":      `UPDATE access_grants SET expires_at=clock_timestamp()-interval '1 second' WHERE organization_id=$1`,
		} {
			t.Run(name, func(t *testing.T) {
				pool, secrets, model, token, err := gatewayRelease(t, true)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(t.Context(), mutate, model.Invoke.OrganizationID); err != nil {
					t.Fatal(err)
				}
				tx, err := pool.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				_, direct := access.AuthorizeModelInvoke(t.Context(), tx, model.Invoke)
				_ = tx.Rollback(t.Context())
				_, viaGateway := authorize(t, pool, secrets, token)
				if !errors.Is(direct, access.ErrDenied) || !errors.Is(viaGateway, gateway.ErrDenied) {
					t.Fatalf("parity: AuthorizeModelInvoke=%v gateway=%v", direct, viaGateway)
				}
			})
		}
	})
	t.Run("master switch", func(t *testing.T) {
		// Off at release: a brokered attempt fails closed, never falls back to the raw key.
		if _, _, _, token, err := gatewayRelease(t, false); !errors.Is(err, gateway.ErrDisabled) || token != "" {
			t.Fatalf("disabled gateway released %q: %v", token, err)
		}
		// Turned off mid-run: the next request is rejected with a clear error.
		pool, secrets, model, token, err := gatewayRelease(t, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE gateway_org_settings SET enabled=false WHERE organization_id=$1`,
			model.Invoke.OrganizationID); err != nil {
			t.Fatal(err)
		}
		if _, err := authorize(t, pool, secrets, token); !errors.Is(err, gateway.ErrDisabled) {
			t.Fatalf("disabled gateway authorized: %v", err)
		}
	})
	t.Run("brokered grant never releases the raw key", func(t *testing.T) {
		pool, ledger, secrets, model, runtime, _ := modelAttemptFixture(t)
		for _, statement := range []string{
			`UPDATE access_grants SET delivery_mode='brokered_gateway' WHERE organization_id=$1`,
			`UPDATE access_project_policies SET delivery_modes=ARRAY['brokered_gateway'] WHERE organization_id=$1`,
			`UPDATE access_provider_registrations SET delivery_modes=ARRAY['brokered_gateway'] WHERE organization_id=$1`,
		} {
			if _, err := pool.Exec(t.Context(), statement, model.Invoke.OrganizationID); err != nil {
				t.Fatal(err)
			}
		}
		redeemed := modelPhaseFixture(t, pool, ledger, model)
		connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model) // no Gateway hook
		if err != nil {
			t.Fatal(err)
		}
		reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
		_, err = ledger.Release(t.Context(), redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
			if err := connector.Authorize(ctx, tx, runtime, redeemed.Challenge); err != nil {
				return err
			}
			_, err := connector.ModelCredential(ctx, tx, runtime)
			return err
		})
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("brokered grant released a raw key: %v", err)
		}
	})
}
