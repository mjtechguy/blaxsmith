package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
)

func TestSessionLifecyclePostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := context.Background()
	password := []byte("correct horse battery staple")
	owner, err := BootstrapOwner(ctx, pool, "alice", "engineering", "Engineering", password)
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewSessionManager(pool, "blaxsmith-test", signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.LoginLocal(ctx, "engineering", "alice", []byte("wrong password")); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong password accepted: %v", err)
	}
	if _, err := manager.LoginLocal(ctx, "engineering", "unknown", password); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown account accepted: %v", err)
	}
	first, err := manager.LoginLocal(ctx, "engineering", "alice", password)
	if err != nil {
		t.Fatal(err)
	}
	caller, err := manager.ValidateAccess(ctx, first.Access)
	if err != nil || caller.OrganizationID != owner.OrganizationID || caller.PrincipalID != owner.PrincipalID || caller.Role != "owner" {
		t.Fatalf("bad access identity: %+v, %v", caller, err)
	}
	if _, err := manager.ValidateAccess(ctx, "bad-token"); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("bad token accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='viewer'
		WHERE organization_id=$1 AND principal_id=$2`, owner.OrganizationID, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateAccess(ctx, first.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("stale owner role accepted: %v", err)
	}
	second, err := manager.Refresh(ctx, first.Refresh)
	if err != nil || second.Role != "viewer" || second.Refresh == first.Refresh {
		t.Fatalf("refresh failed to pick up new role: %+v, %v", second, err)
	}
	if _, err := manager.ValidateAccess(ctx, second.Access); err != nil {
		t.Fatalf("rotated access rejected: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET mfa_policy='required' WHERE id=$1`, owner.OrganizationID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateAccess(ctx, second.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("required MFA bypassed: %v", err)
	}
	if _, err := manager.Refresh(ctx, second.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("required MFA bypassed on refresh: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET mfa_policy='optional' WHERE id=$1`, owner.OrganizationID); err != nil {
		t.Fatal(err)
	}
	third, err := manager.Refresh(ctx, second.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Refresh(ctx, second.Refresh); !errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("refresh replay did not revoke session: %v", err)
	}
	if _, err := manager.ValidateAccess(ctx, third.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replayed session still accepted: %v", err)
	}
	if _, err := manager.Refresh(ctx, third.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("replayed session still refreshes: %v", err)
	}

	last, err := manager.LoginLocal(ctx, "engineering", "alice", password)
	if err != nil {
		t.Fatal(err)
	}
	caller, err = manager.ValidateAccess(ctx, last.Access)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Revoke(ctx, caller); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateAccess(ctx, last.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked access accepted: %v", err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events
		WHERE organization_id=$1`, owner.OrganizationID).Scan(&audits); err != nil || audits != 7 {
		t.Fatalf("session audit count: %d, %v", audits, err)
	}
}
