package identity

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
)

func TestLoginLimitPostgres(t *testing.T) {
	pool := identityTestPool(t)
	limit, err := NewLoginLimit(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := netip.MustParseAddr("192.0.2.1")
	var wg sync.WaitGroup
	results := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- limit.Allow(ctx, "engineering", "alice", source)
		}()
	}
	wg.Wait()
	close(results)
	allowed, denied := 0, 0
	for err := range results {
		switch {
		case err == nil:
			allowed++
		case errors.Is(err, ErrRateLimited):
			denied++
		default:
			t.Fatal(err)
		}
	}
	if allowed != 10 || denied != 10 {
		t.Fatalf("shared account limit: %d allowed, %d denied", allowed, denied)
	}
	if err := limit.Allow(ctx, "engineering", "alice", netip.MustParseAddr("192.0.2.2")); err != nil {
		t.Fatalf("another source was blocked: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_login_limits SET window_start=clock_timestamp()-interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	if err := limit.Allow(ctx, "engineering", "alice", source); err != nil {
		t.Fatalf("expired window did not reset: %v", err)
	}
	if err := limit.Allow(ctx, "engineering", "alice", netip.Addr{}); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("invalid source was accepted: %v", err)
	}
	other := netip.MustParseAddr("192.0.2.3")
	for i := range 60 {
		if err := limit.Allow(ctx, "engineering", fmt.Sprintf("user%03d", i), other); err != nil {
			t.Fatalf("source blocked early at attempt %d: %v", i, err)
		}
	}
	if err := limit.Allow(ctx, "engineering", "lastuser", other); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("source limit was bypassed: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_login_limits SET window_start=clock_timestamp()-interval '2 days'`); err != nil {
		t.Fatal(err)
	}
	if err := limit.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_login_limits`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("old limit keys remain: %d, %v", remaining, err)
	}
}

func TestLoginLimitAcrossSourcesPostgres(t *testing.T) {
	pool := identityTestPool(t)
	first, err := NewLoginLimit(pool)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewLoginLimit(pool)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			limiter := first
			if i%2 == 1 {
				limiter = second
			}
			results <- limiter.Allow(context.Background(), "engineering", "alice", netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}))
		}()
	}
	wg.Wait()
	close(results)
	allowed, denied := 0, 0
	for err := range results {
		switch {
		case err == nil:
			allowed++
		case errors.Is(err, ErrRateLimited):
			denied++
		default:
			t.Fatal(err)
		}
	}
	if allowed != 30 || denied != 10 {
		t.Fatalf("distributed account limit: %d allowed, %d denied", allowed, denied)
	}
	if err := first.Allow(context.Background(), "engineering", "bob", netip.MustParseAddr("192.0.2.1")); err != nil {
		t.Fatalf("other account was blocked: %v", err)
	}
	if err := first.Allow(context.Background(), "other-org", "alice", netip.MustParseAddr("192.0.2.1")); err != nil {
		t.Fatalf("same username in another organization was blocked: %v", err)
	}
}
