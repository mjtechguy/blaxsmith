package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestOperatorResetLinkPostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := tenant.System(context.Background())
	if _, err := BootstrapOwner(ctx, pool, "owner@example.com", "engineering", "Engineering", []byte("forgotten password 1")); err != nil {
		t.Fatal(err)
	}
	if _, err := OperatorResetLink(ctx, pool, "nobody@example.com", ""); !errors.Is(err, ErrUserNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	if _, err := OperatorResetLink(ctx, pool, "owner@example.com", "other-org"); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("wrong organization: %v", err)
	}
	first, err := OperatorResetLink(ctx, pool, "Owner@Example.com", "")
	if err != nil || first.Purpose != "reset" || len(first.Token) != 43 {
		t.Fatalf("reset link: %+v %v", first, err)
	}
	link, err := OperatorResetLink(ctx, pool, "owner@example.com", "engineering")
	if err != nil {
		t.Fatal(err)
	}
	users, err := NewUserAdmin(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := users.CompleteLink(ctx, first.Token, []byte("new owner password 2"), "", ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("superseded link still works: %v", err)
	}
	if _, err := users.CompleteLink(ctx, link.Token, []byte("new owner password 2"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := users.CompleteLink(ctx, link.Token, []byte("new owner password 3"), "", ""); !errors.Is(err, ErrLinkInvalid) {
		t.Fatalf("link reused: %v", err)
	}
	_, signer, _ := ed25519.GenerateKey(rand.Reader)
	manager, err := NewSessionManager(pool, "blaxsmith-test", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LoginLocal(ctx, "engineering", "owner@example.com", []byte("new owner password 2"), netip.MustParseAddr("192.0.2.1"), ""); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE actor_kind='operator'
		AND action='identity.user.reset_link_issued'`).Scan(&audited); err != nil || audited != 2 {
		t.Fatalf("operator audit events: %d %v", audited, err)
	}
}
