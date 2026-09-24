package identity

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadSessionSigner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.seed")
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := LoadSessionSigner(path)
	if err != nil || !bytes.Equal(signer, ed25519.NewKeyFromSeed(seed)) {
		t.Fatalf("valid signer rejected: %v", err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionSigner(path); !errors.Is(err, ErrSessionConfiguration) {
		t.Fatalf("world-readable signer accepted: %v", err)
	}
	if err := os.WriteFile(path, seed[:16], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionSigner(path); !errors.Is(err, ErrSessionConfiguration) {
		t.Fatalf("short signer accepted: %v", err)
	}
}

func TestLoadSessionPublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "previous.pub")
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, public, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionPublicKey(path)
	if err != nil || !bytes.Equal(loaded, public) {
		t.Fatalf("valid previous public key rejected: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPublicKey(path); !errors.Is(err, ErrSessionConfiguration) {
		t.Fatalf("writable previous public key accepted: %v", err)
	}
	if err := os.WriteFile(path, public[:16], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSessionPublicKey(path); !errors.Is(err, ErrSessionConfiguration) {
		t.Fatalf("short previous public key accepted: %v", err)
	}
}

func TestSessionLifecyclePostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := context.Background()
	password := []byte("correct horse battery staple")
	owner, err := BootstrapOwner(ctx, pool, "alice@example.com", "engineering", "Engineering", password)
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
	source := netip.MustParseAddr("192.0.2.1")
	if _, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", []byte("wrong password"), source, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("wrong password accepted: %v", err)
	}
	if _, err := manager.LoginLocal(ctx, "engineering", "unknown@example.com", password, source, ""); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("unknown account accepted: %v", err)
	}
	first, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, source, "")
	if err != nil {
		t.Fatal(err)
	}
	_, nextSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewSessionManager(pool, "blaxsmith-test", nextSigner, signer.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.ValidateAccess(ctx, first.Access); err != nil {
		t.Fatalf("previous signing key rejected during overlap: %v", err)
	}
	withoutPrevious, err := NewSessionManager(pool, "blaxsmith-test", nextSigner)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutPrevious.ValidateAccess(ctx, first.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("untrusted signing key accepted: %v", err)
	}
	newAccess, _, err := rotated.sign(time.Now().UTC(), first.Organization, first.Principal, first.SessionID, first.Role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rotated.ValidateAccess(ctx, newAccess); err != nil {
		t.Fatalf("rotated signing key rejected: %v", err)
	}
	if _, err := manager.ValidateAccess(ctx, newAccess); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("new signing key accepted without trust: %v", err)
	}
	corrupt := append(ed25519.PrivateKey(nil), signer...)
	corrupt[len(corrupt)-1] ^= 1
	if _, err := NewSessionManager(pool, "blaxsmith-test", corrupt); !errors.Is(err, ErrSessionConfiguration) {
		t.Fatalf("corrupt signing key accepted: %v", err)
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

	last, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, source, "")
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
	final, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RevokeRefresh(ctx, final.Refresh); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ValidateAccess(ctx, final.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("refresh-token logout left access valid: %v", err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events
		WHERE organization_id=$1`, owner.OrganizationID).Scan(&audits); err != nil || audits != 9 {
		t.Fatalf("session audit count: %d, %v", audits, err)
	}
}
