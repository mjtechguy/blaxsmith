package access

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// RenewedLease is a model lease whose expiry moved forward.
type RenewedLease struct {
	OrganizationID, AttemptID, LeaseID string
	DeliveryMode                       string // grant delivery mode, e.g. native_raw
	ExpiresAt                          time.Time
}

// RenewModelLeases keeps each delivered model lease short (ttl ≤ 1h) but
// extends it while the attempt stays authorized: the grant, policy,
// connection, and secret version still allow it, and the attempt is still
// running (autonomous or taken over) under the lease's owner generation. A
// lease within ttl/2 of expiry is extended to now+ttl. Revoked or expired
// leases are never renewed; the worker then stops at the old expiry.
func RenewModelLeases(ctx context.Context, db *pgxpool.Pool, ttl time.Duration) ([]RenewedLease, error) {
	ctx = tenant.System(ctx)
	if db == nil || ttl <= 0 || ttl > time.Hour {
		return nil, ErrDenied
	}
	rows, err := db.Query(ctx, `UPDATE access_leases l SET expires_at=clock_timestamp()+$1::interval
		FROM access_bindings b, access_grants g, access_project_policies p, access_connections c, access_secret_versions sv,
			workflow_attempts a
		WHERE l.capability='model.invoke' AND l.revoked_at IS NULL AND l.delivered_at IS NOT NULL
		AND l.expires_at>clock_timestamp() AND l.expires_at<clock_timestamp()+$1::interval/2
		AND b.organization_id=l.organization_id AND b.id=l.binding_id AND b.capability='model.invoke'
		AND g.organization_id=b.organization_id AND g.id=b.grant_id AND g.revoked_at IS NULL
		AND g.version=b.grant_version AND (g.expires_at IS NULL OR g.expires_at>=clock_timestamp()+$1::interval)
		AND p.organization_id=b.organization_id AND p.project_id=b.project_id AND p.version=b.policy_version
		AND c.organization_id=g.organization_id AND c.id=g.connection_id AND c.id=l.connection_id AND c.state='active'
		AND sv.organization_id=c.organization_id AND sv.connection_id=c.id AND sv.version=c.active_secret_version
		AND (sv.expires_at IS NULL OR sv.expires_at>=clock_timestamp()+$1::interval)
		AND a.organization_id::text=l.organization_id AND a.id::text=l.attempt_id
		AND a.state='running' AND a.generation=l.owner_generation
		RETURNING l.organization_id,l.attempt_id,l.id,g.delivery_mode,l.expires_at`, ttl)
	if err != nil {
		return nil, fmt.Errorf("renew model leases: %w", err)
	}
	defer rows.Close()
	var out []RenewedLease
	for rows.Next() {
		var r RenewedLease
		if err := rows.Scan(&r.OrganizationID, &r.AttemptID, &r.LeaseID, &r.DeliveryMode, &r.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
