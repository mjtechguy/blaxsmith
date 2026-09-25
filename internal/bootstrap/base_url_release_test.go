package bootstrap

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// A connection's base URL is frozen into the attempt's binding. The key is
// released as usual (it is still the connection's own API key); changing the
// base URL afterwards fences the attempt: no release, no renewal.
func TestModelBaseURLIsFrozenAndFencedPostgres(t *testing.T) {
	const base = "https://litellm.example.com/v1"
	pool, ledger, secrets, model, runtime, _ := modelAttemptFixtureWith(t, modelFixtureSpec{baseURL: base})
	ctx := tenant.System(t.Context())
	frozen, err := access.AttemptModelBaseURL(ctx, pool, model.Invoke.OrganizationID, model.Attempt.ID)
	if err != nil || frozen != base {
		t.Fatalf("frozen base URL: %q %v", frozen, err)
	}
	redeemed := modelPhaseFixture(t, pool, ledger, model)
	connector, err := NewModelAttemptConnector(Connector{Ledger: ledger}, pool, secrets, model)
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(ctx context.Context, tx pgx.Tx) error { return connector.Reserve(ctx, tx, redeemed) }
	_, err = ledger.Release(ctx, redeemed, reserve, func(ctx context.Context, tx pgx.Tx) error {
		if err := connector.Authorize(ctx, tx, runtime, redeemed.Challenge); err != nil {
			return err
		}
		decision, err := access.AuthorizeModelInvoke(ctx, tx, model.Invoke)
		if err != nil || decision.BaseURL != base {
			return errors.Join(err, errors.New("decision lost the base URL"))
		}
		credential, err := connector.ModelCredential(ctx, tx, runtime)
		if err != nil {
			return err
		}
		defer clear(credential.APIKey)
		if string(credential.APIKey) != "private-provider-key" {
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
	if renewed, err := access.RenewModelLeases(ctx, pool, 30*time.Minute); err != nil || len(renewed) != 1 {
		t.Fatalf("renewal with an unchanged base URL: %+v %v", renewed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE access_leases SET expires_at=clock_timestamp()+interval '1 minute'
		WHERE attempt_id=$1`, model.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE access_connections SET base_url='https://elsewhere.example.com/v1'
		WHERE organization_id=$1 AND id='connection'`, model.Invoke.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if renewed, err := access.RenewModelLeases(ctx, pool, 30*time.Minute); err != nil || len(renewed) != 0 {
		t.Fatalf("a changed base URL still renewed: %+v %v", renewed, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := access.AuthorizeModelInvoke(ctx, tx, model.Invoke); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("a changed base URL still authorized release: %v", err)
	}
}

// Subscriptions never carry a base URL: the database refuses one.
func TestSubscriptionRefusesBaseURLPostgres(t *testing.T) {
	pool, _, _, model, _, _ := modelAttemptFixtureWith(t, modelFixtureSpec{provider: "anthropic", model: "claude-sonnet-5",
		origin: "https://api.anthropic.com", authMethod: access.ClaudeSetupTokenAuth, key: "sk-ant-oat01-owner",
		ownerKind: "user", owner: "alice", granteeKind: "user", grantee: "alice"})
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE access_connections SET base_url='https://litellm.example.com'
		WHERE organization_id=$1 AND id='connection'`, model.Invoke.OrganizationID); err == nil {
		t.Fatal("a subscription accepted a base URL")
	}
}
