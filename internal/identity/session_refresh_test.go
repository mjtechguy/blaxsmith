package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// expireGrace ages every consumed refresh token past the grace window.
func expireGrace(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(tenant.System(context.Background()), `UPDATE identity_refresh_tokens
		SET consumed_at=consumed_at-interval '2 minutes' WHERE consumed_at IS NOT NULL`); err != nil {
		t.Fatal(err)
	}
}

func refreshFixture(t *testing.T) (*pgxpool.Pool, *SessionManager, func() Tokens) {
	t.Helper()
	pool := identityTestPool(t)
	ctx := tenant.System(context.Background())
	password := []byte("correct horse battery staple")
	if _, err := BootstrapOwner(ctx, pool, "alice@example.com", "engineering", "Engineering", password); err != nil {
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
	login := func() Tokens {
		t.Helper()
		tokens, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, netip.MustParseAddr("192.0.2.1"), "")
		if err != nil {
			t.Fatal(err)
		}
		return tokens
	}
	return pool, manager, login
}

func TestRefreshGraceWindowPostgres(t *testing.T) {
	pool, manager, login := refreshFixture(t)
	ctx := tenant.System(context.Background())
	first := login()
	winner, err := manager.Refresh(ctx, first.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	// A second tab, or a retry whose response was lost to a restart, replays
	// the rotated-out token inside the grace window: same successor.
	late, err := manager.Refresh(ctx, first.Refresh)
	if err != nil {
		t.Fatalf("replay inside the grace window failed: %v", err)
	}
	if late.Refresh != winner.Refresh || late.SessionID != first.SessionID || late.Access == "" {
		t.Fatalf("grace replay did not return the same successor session: %+v vs %+v", late, winner)
	}
	if _, err := manager.ValidateAccess(ctx, late.Access); err != nil {
		t.Fatalf("grace access token rejected: %v", err)
	}
	var live int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_refresh_tokens
		WHERE session_id=$1 AND consumed_at IS NULL`, first.SessionID).Scan(&live); err != nil || live != 1 {
		t.Fatalf("grace replay minted extra refresh tokens: %d, %v", live, err)
	}
	// Once the successor has itself been used, the old token is theft-shaped
	// even inside the window.
	next, err := manager.Refresh(ctx, winner.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Refresh(ctx, first.Refresh); !errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("replay after the successor was used was accepted: %v", err)
	}
	if _, err := manager.ValidateAccess(ctx, next.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("reuse did not revoke the session: %v", err)
	}
	if _, err := manager.Refresh(ctx, next.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session still refreshes: %v", err)
	}

	// After the window, a replay revokes the family even though the
	// successor is unused.
	second := login()
	rotated, err := manager.Refresh(ctx, second.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	expireGrace(t, pool)
	if _, err := manager.Refresh(ctx, second.Refresh); !errors.Is(err, ErrRefreshReuse) {
		t.Fatalf("replay after the grace window was accepted: %v", err)
	}
	if _, err := manager.Refresh(ctx, rotated.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("successor survived reuse detection: %v", err)
	}
	var actions []string
	rows, err := pool.Query(ctx, `SELECT action FROM identity_audit_events
		WHERE action LIKE 'identity.refresh%' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var action string
		if err := rows.Scan(&action); err != nil {
			t.Fatal(err)
		}
		actions = append(actions, action)
	}
	want := []string{"identity.refresh", "identity.refresh_grace", "identity.refresh", "identity.refresh_reuse",
		"identity.refresh", "identity.refresh_reuse"}
	if len(actions) != len(want) {
		t.Fatalf("refresh audit trail: %v", actions)
	}
	for i := range want {
		if actions[i] != want[i] {
			t.Fatalf("refresh audit trail: %v", actions)
		}
	}
}

func TestConcurrentRefreshPostgres(t *testing.T) {
	_, manager, login := refreshFixture(t)
	ctx := tenant.System(context.Background())
	first := login()
	const tabs = 6
	results := make([]Tokens, tabs)
	errs := make([]error, tabs)
	var wg sync.WaitGroup
	for i := range tabs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = manager.Refresh(ctx, first.Refresh)
		}()
	}
	wg.Wait()
	for i := range tabs {
		if errs[i] != nil {
			t.Fatalf("concurrent refresh %d failed: %v", i, errs[i])
		}
		if results[i].Refresh != results[0].Refresh || results[i].SessionID != first.SessionID {
			t.Fatalf("concurrent refreshes diverged: %q vs %q", results[i].Refresh, results[0].Refresh)
		}
	}
	if _, err := manager.Refresh(ctx, results[0].Refresh); err != nil {
		t.Fatalf("shared successor rejected: %v", err)
	}
}

func TestSessionIdleAndAbsoluteExpiryPostgres(t *testing.T) {
	pool, manager, login := refreshFixture(t)
	ctx := tenant.System(context.Background())
	for _, bad := range []SessionPolicy{{Idle: time.Minute, Absolute: time.Hour}, {Idle: 2 * time.Hour, Absolute: time.Hour},
		{Idle: time.Hour, Absolute: 400 * 24 * time.Hour}} {
		if err := manager.SetPolicy(bad); !errors.Is(err, ErrSessionConfiguration) {
			t.Fatalf("invalid policy %+v accepted: %v", bad, err)
		}
	}
	if err := manager.SetPolicy(SessionPolicy{Idle: time.Hour, Absolute: 3 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	expiresAt := func(session string) time.Time {
		t.Helper()
		var at time.Time
		if err := pool.QueryRow(ctx, `SELECT expires_at FROM identity_sessions WHERE id=$1`, session).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	near := func(got, want time.Time) bool { return got.Sub(want).Abs() < time.Minute }

	// Sign-in lasts the idle timeout; each refresh slides it forward.
	idle := login()
	if at := expiresAt(idle.SessionID); !near(at, time.Now().Add(time.Hour)) || !near(idle.SessionExpires, at) {
		t.Fatalf("sign-in expiry %v, want about an hour", at)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET expires_at=clock_timestamp()+interval '5 minutes' WHERE id=$1`,
		idle.SessionID); err != nil {
		t.Fatal(err)
	}
	slid, err := manager.Refresh(ctx, idle.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if at := expiresAt(idle.SessionID); !near(at, time.Now().Add(time.Hour)) || !near(slid.SessionExpires, at) {
		t.Fatalf("refresh did not slide expiry: %v", at)
	}
	// Idle past the timeout: refresh and access both fail.
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`,
		idle.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Refresh(ctx, slid.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("idle-expired session refreshed: %v", err)
	}
	if _, err := manager.ValidateAccess(ctx, slid.Access); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("idle-expired session accepted: %v", err)
	}

	// Near the absolute limit a refresh extends only up to it.
	capped := login()
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET created_at=clock_timestamp()-interval '150 minutes' WHERE id=$1`,
		capped.SessionID); err != nil {
		t.Fatal(err)
	}
	last, err := manager.Refresh(ctx, capped.Refresh)
	if err != nil {
		t.Fatal(err)
	}
	if at := expiresAt(capped.SessionID); !near(at, time.Now().Add(30*time.Minute)) || !near(last.SessionExpires, at) {
		t.Fatalf("refresh passed the absolute limit: %v", at)
	}
	// Past the absolute limit, activity does not help.
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET created_at=clock_timestamp()-interval '181 minutes' WHERE id=$1`,
		capped.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Refresh(ctx, last.Refresh); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("session refreshed past its absolute lifetime: %v", err)
	}
}

func TestCheckSessionOutlivesAccessTokenPostgres(t *testing.T) {
	_, manager, login := refreshFixture(t)
	ctx := tenant.System(context.Background())
	tokens := login()
	caller, err := manager.ValidateAccess(ctx, tokens.Access)
	if err != nil {
		t.Fatal(err)
	}
	caller.AccessExpires = time.Now().Add(-time.Hour) // the stream's token has long expired
	if _, err := manager.CheckSession(ctx, caller); err != nil {
		t.Fatalf("live session rejected on recheck: %v", err)
	}
	changed := caller
	changed.Role = "viewer"
	if _, err := manager.CheckSession(ctx, changed); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("role change survived recheck: %v", err)
	}
	if err := manager.Revoke(ctx, caller); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CheckSession(ctx, caller); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("revoked session survived recheck: %v", err)
	}
}
