package access

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Two stores on one database stand in for two replicas: a sign-in started on
// one finishes on the other, once, only for the session that started it, and
// the secret payload is never stored in plaintext.
func TestPendingSignInAcrossReplicasPostgres(t *testing.T) {
	ctx := tenant.System(context.Background())
	pool := testPool(t)
	keys := map[string][]byte{"key": []byte(strings.Repeat("k", 32))}
	one, err := NewSecretStore(pool, "key", keys)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewSecretStore(pool, "key", keys)
	if err != nil {
		t.Fatal(err)
	}
	alice := PendingOwner{OrganizationID: "org", PrincipalID: "alice", SessionID: "s1"}
	secret := []byte(`{"verifier":"pkce-verifier-secret"}`)
	if err := one.SavePending(ctx, "github_oauth", "state-1", alice, secret, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	var stored []byte
	if err := pool.QueryRow(ctx, `SELECT ciphertext FROM access_pending_sign_ins`).Scan(&stored); err != nil ||
		bytes.Contains(stored, []byte("pkce-verifier-secret")) {
		t.Fatalf("pending payload not encrypted: %v", err)
	}
	owner, payload, err := two.TakePending(ctx, "github_oauth", "state-1")
	if err != nil || owner != alice || !bytes.Equal(payload, secret) {
		t.Fatalf("take on another replica: %+v %s %v", owner, payload, err)
	}
	if _, _, err := one.TakePending(ctx, "github_oauth", "state-1"); !errors.Is(err, ErrDenied) {
		t.Fatalf("state reused: %v", err)
	}

	// Device polling: one claim at a time, bound to the starting session.
	if err := one.SavePending(ctx, "codex_device", "login-1", alice, []byte("device code"), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := two.ClaimPending(ctx, "codex_device", "login-1", PendingOwner{"org", "alice", "s2"}, time.Minute); !errors.Is(err, ErrDenied) {
		t.Fatalf("other session claimed: %v", err)
	}
	if got, err := two.ClaimPending(ctx, "codex_device", "login-1", alice, time.Minute); err != nil || string(got) != "device code" {
		t.Fatalf("claim: %s %v", got, err)
	}
	if _, err := one.ClaimPending(ctx, "codex_device", "login-1", alice, time.Minute); !errors.Is(err, ErrPendingBusy) {
		t.Fatalf("concurrent claim: %v", err)
	}
	if err := two.FinishPending(ctx, "codex_device", "login-1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := one.ClaimPending(ctx, "codex_device", "login-1", alice, time.Minute); err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if err := one.FinishPending(ctx, "codex_device", "login-1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := two.ClaimPending(ctx, "codex_device", "login-1", alice, time.Minute); !errors.Is(err, ErrDenied) {
		t.Fatalf("finished sign-in claimed: %v", err)
	}

	// Expired rows are refused and swept by the next save.
	if err := one.SavePending(ctx, "github_oauth", "old", alice, secret, time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := one.SavePending(ctx, "github_oauth", "new", alice, secret, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := two.TakePending(ctx, "github_oauth", "old"); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired state accepted: %v", err)
	}
	for i := 0; ; i++ {
		err := one.SavePending(ctx, "github_oauth", "cap-"+string(rune('a'+i)), alice, secret, time.Now().Add(time.Minute))
		if errors.Is(err, ErrTooManyPending) {
			break
		}
		if err != nil || i > maxPendingPerPrincipal {
			t.Fatalf("pending cap: %d %v", i, err)
		}
	}
}
