package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

func TestClaudeSubscriptionIsOwnerOnlyAndOrgGated(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	org := organization(t, pool, "claude-sub")
	owner := reviewer(t, pool, org, "owner", "claude-sub-owner")
	alice := reviewer(t, pool, org, "member", "claude-sub-alice")
	bob := reviewer(t, pool, org, "member", "claude-sub-bob")
	secrets := connectionSecrets(t, store)
	token := []byte("sk-ant-oat01-" + strings.Repeat("a", 90))

	// Off by default: nobody can connect, and members can't flip the switch.
	if allowed, err := store.ClaudeSubscriptionAllowed(ctx, alice); err != nil || allowed {
		t.Fatalf("switch should default off: %v %v", allowed, err)
	}
	if _, err := store.CreateClaudeSubscriptionAs(ctx, alice, token, secrets); !errors.Is(err, access.ErrClaudeSubscriptionDisabled) {
		t.Fatalf("connected while disabled: %v", err)
	}
	if err := store.SetClaudeSubscriptionAllowedAs(ctx, alice, true); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member flipped the switch: %v", err)
	}
	if err := store.SetClaudeSubscriptionAllowedAs(ctx, owner, true); err != nil {
		t.Fatal(err)
	}
	var enabledAudits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND action='access.policy.claude_subscription_enabled'`, org).Scan(&enabledAudits); err != nil || enabledAudits != 1 {
		t.Fatalf("switch not audited once: %d %v", enabledAudits, err)
	}

	// Only the setup-token shape is accepted; the value never comes back.
	for _, bad := range [][]byte{[]byte("sk-ant-api03-" + strings.Repeat("a", 40)), []byte("sk-ant-oat01-has space"), nil} {
		if _, err := store.CreateClaudeSubscriptionAs(ctx, alice, bad, secrets); !errors.Is(err, ErrInvalid) {
			t.Fatalf("bad token %q accepted: %v", bad, err)
		}
	}
	c, err := store.CreateClaudeSubscriptionAs(ctx, alice, token, secrets)
	if err != nil || c.Scope != ScopePersonal || c.OwnerID != alice.PrincipalID || c.Kind != "subscription" {
		t.Fatalf("personal Claude subscription: %+v %v", c, err)
	}
	if raw, _ := json.Marshal(c); strings.Contains(string(raw), "sk-ant-oat") {
		t.Fatalf("connection response leaks the token: %s", raw)
	}

	// Only its owner can use it in a project, as a user grant.
	project, err := store.CreateProjectAs(ctx, alice, "claude-sub-proj", "Claude sub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionUseAs(ctx, bob, c.ID, project, "claude-opus-4-6"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another member used Alice's subscription: %v", err)
	}
	use, err := store.AddConnectionUseAs(ctx, alice, c.ID, project, "claude-opus-4-6")
	if err != nil || use.GranteeKind != "user" {
		t.Fatalf("owner use: %+v %v", use, err)
	}
	var grantee, mode string
	if err := pool.QueryRow(ctx, `SELECT grantee_id,delivery_mode FROM access_grants WHERE organization_id=$1 AND id=$2`,
		org, use.ID).Scan(&grantee, &mode); err != nil || grantee != alice.PrincipalID || mode != "native_raw" {
		t.Fatalf("grant is not Alice's own native_raw grant: %q %q %v", grantee, mode, err)
	}

	// Turning the switch off blocks new uses (and delivery, in access).
	if err := store.SetClaudeSubscriptionAllowedAs(ctx, owner, false); err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateProjectAs(ctx, alice, "claude-sub-proj2", "Claude sub 2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionUseAs(ctx, alice, c.ID, other, "claude-opus-4-6"); !errors.Is(err, access.ErrClaudeSubscriptionDisabled) {
		t.Fatalf("new use while disabled: %v", err)
	}
}
