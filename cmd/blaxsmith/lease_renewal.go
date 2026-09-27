package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const (
	leaseExpiresPath = "/tmp/blaxsmith/lease-expires"
	// The worker records where it placed the Codex sign-in (tooladapter.CodexAuthPathFile).
	codexAuthPathFile = "/tmp/blaxsmith/" + tooladapter.CodexAuthPathFile
)

// codexAuthPath is the only shape the recorded path may have: auth.json in
// the attempt's temp home's CODEX_HOME. The guest cannot steer the write
// anywhere else.
var codexAuthPath = regexp.MustCompile(`^/tmp/blaxsmith-tool-[0-9]+/\.codex/auth\.json$`)

// guestFiles is the part of *terminal.Guest the renewal hooks use.
type guestFiles interface {
	ReadFile(ctx context.Context, path string, max int) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, mode uint32) error
}

// deliveryRenewer refreshes a renewed lease's delivered credential payload in
// the guest before the new expiry is announced, and returns the expiry to
// announce. Raw API keys (native_raw) never change, so theirs is a no-op.
type deliveryRenewer func(context.Context, access.RenewedLease, guestFiles) (time.Time, error)

func deliveryRenewers(oauth *access.OAuthRefresher) map[string]deliveryRenewer {
	renewers := map[string]deliveryRenewer{
		"native_raw": func(_ context.Context, lease access.RenewedLease, _ guestFiles) (time.Time, error) {
			return lease.ExpiresAt, nil
		},
	}
	if oauth != nil {
		renewers["oauth_access"] = codexRenewer(oauth.RenewLease)
	}
	return renewers
}

// codexRenewer pushes a freshly authorized Codex sign-in (access and id
// tokens, empty refresh token) over the worker's $CODEX_HOME/auth.json in
// place, and announces at most the token's expiry minus the Codex margin.
func codexRenewer(renew func(context.Context, access.RenewedLease) (access.Delivery, time.Time, error)) deliveryRenewer {
	return func(ctx context.Context, lease access.RenewedLease, guest guestFiles) (time.Time, error) {
		raw, err := guest.ReadFile(ctx, codexAuthPathFile, 256)
		if err != nil {
			return time.Time{}, fmt.Errorf("read Codex sign-in path: %w", err)
		}
		path := strings.TrimSuffix(string(raw), "\n")
		if !codexAuthPath.MatchString(path) {
			return time.Time{}, errors.New("guest recorded an invalid Codex sign-in path")
		}
		delivery, expiresAt, err := renew(ctx, lease)
		if err != nil {
			return time.Time{}, err
		}
		defer delivery.Clear()
		if limit := delivery.ExpiresAt.Add(-access.CodexLeaseMargin); expiresAt.After(limit) {
			expiresAt = limit
		}
		if !expiresAt.After(time.Now()) {
			return time.Time{}, access.ErrDenied
		}
		if _, err := tooladapter.ParseCodexAuth(delivery.File); err != nil {
			return time.Time{}, err // never push a refresh-token-bearing file
		}
		// WriteFile truncates the worker-owned file in place (same inode, owner, 0600).
		if err := guest.WriteFile(ctx, path, delivery.File, 0o600); err != nil {
			return time.Time{}, err
		}
		return expiresAt, nil
	}
}

// leaseRenewer extends still-authorized model leases on the dispatch leader's
// tick and tells each worker its new expiry through a guest file.
// ponytail: renewal is platform-push. A lease that is revoked or not renewed
// runs out and the worker stops the harness at the old expiry; revocation
// also takes the existing stop-actor path, which removes the raw key.
func leaseRenewer(pool *pgxpool.Pool, store *workflow.Store, guests *terminal.Router, ttl time.Duration,
	oauth *access.OAuthRefresher) func(context.Context) {
	renewers := deliveryRenewers(oauth)
	var after [2]string
	return func(ctx context.Context) {
		renewed, err := access.RenewModelLeases(ctx, pool, ttl, after[0], after[1], 10)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("model lease renewal: %v", err)
			}
			return
		}
		after = deliverLeasePage(ctx, after, renewed, func(ctx context.Context, lease access.RenewedLease) error {
			t, err := store.GetAttemptTerminal(ctx, lease.OrganizationID, lease.AttemptID)
			var guest *terminal.Guest
			if err == nil {
				guest, err = guests.Guest(t.Atespace, t.Actor)
			}
			if err == nil {
				err = announceRenewal(ctx, renewers, lease, guest)
			}
			if err == nil {
				err = access.MarkRenewalDelivered(ctx, pool, lease)
			}
			return err
		})
	}
}

// Advance past each attempted delivery, even if it consumes the pass deadline.
// Unattempted leases remain ahead of the cursor; failures retry after wrapping.
func deliverLeasePage(ctx context.Context, after [2]string, leases []access.RenewedLease,
	deliver func(context.Context, access.RenewedLease) error) [2]string {
	if len(leases) == 0 && ctx.Err() == nil {
		return [2]string{}
	}
	for _, lease := range leases {
		if ctx.Err() != nil {
			break
		}
		after = [2]string{lease.OrganizationID, lease.LeaseID}
		if err := deliver(ctx, lease); err != nil && ctx.Err() == nil {
			log.Printf("model lease renewal for attempt %s pending delivery acknowledgement: %v", lease.AttemptID, err)
		}
	}
	return after
}

// announceRenewal refreshes the delivered payload for the lease's delivery
// mode, then writes the new expiry. An unknown delivery mode is never
// announced: the worker stops at the old expiry.
func announceRenewal(ctx context.Context, renewers map[string]deliveryRenewer, lease access.RenewedLease, guest guestFiles) error {
	refresh, ok := renewers[lease.DeliveryMode]
	if !ok {
		return fmt.Errorf("no renewal for delivery mode %q", lease.DeliveryMode)
	}
	expiresAt, err := refresh(ctx, lease, guest)
	if err != nil {
		return err
	}
	return guest.WriteFile(ctx, leaseExpiresPath, []byte(strconv.FormatInt(expiresAt.Unix(), 10)+"\n"), 0o644)
}
