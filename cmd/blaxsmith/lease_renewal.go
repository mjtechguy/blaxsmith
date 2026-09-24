package main

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const leaseExpiresPath = "/tmp/blaxsmith/lease-expires"

// deliveryRenewer refreshes a renewed lease's delivered credential payload in
// the guest before the new expiry is announced. Raw API keys (native_raw)
// never change, so theirs is a no-op; an OAuth delivery mode plugs its
// refresh (access.RenewOAuthDelivery, from the OAuth lane) in here.
type deliveryRenewer func(context.Context, access.RenewedLease, *terminal.Guest) error

var deliveryRenewers = map[string]deliveryRenewer{
	"native_raw": func(context.Context, access.RenewedLease, *terminal.Guest) error { return nil },
}

// leaseRenewer extends still-authorized model leases on the dispatch leader's
// tick and tells each worker its new expiry through a guest file.
// ponytail: renewal is platform-push. A lease that is revoked or not renewed
// runs out and the worker stops the harness at the old expiry; revocation
// also takes the existing stop-actor path, which removes the raw key.
func leaseRenewer(pool *pgxpool.Pool, store *workflow.Store, guests *terminal.Router, ttl time.Duration) func(context.Context) {
	return func(ctx context.Context) {
		renewed, err := access.RenewModelLeases(ctx, pool, ttl)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("model lease renewal: %v", err)
			}
			return
		}
		for _, lease := range renewed {
			t, err := store.GetAttemptTerminal(ctx, lease.OrganizationID, lease.AttemptID)
			var guest *terminal.Guest
			if err == nil {
				guest, err = guests.Guest(t.Atespace, t.Actor)
			}
			if err == nil {
				// An unknown delivery mode is never announced: the worker stops at the old expiry.
				refresh, ok := deliveryRenewers[lease.DeliveryMode]
				if !ok {
					err = fmt.Errorf("no renewal for delivery mode %q", lease.DeliveryMode)
				} else {
					err = refresh(ctx, lease, guest)
				}
			}
			if err == nil {
				err = guest.WriteFile(ctx, leaseExpiresPath, []byte(strconv.FormatInt(lease.ExpiresAt.Unix(), 10)+"\n"), 0o644)
			}
			if err != nil {
				log.Printf("model lease renewal for attempt %s not delivered: %v", lease.AttemptID, err)
			}
		}
	}
}
