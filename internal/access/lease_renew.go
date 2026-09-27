package access

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// RenewedLease is a model lease awaiting delivery of its renewed expiry.
type RenewedLease struct {
	OrganizationID, AttemptID, LeaseID string
	DeliveryMode                       string // grant delivery mode, e.g. native_raw
	ExpiresAt                          time.Time
	Generation                         int64
}

// RenewModelLeases keeps each delivered model lease short (ttl ≤ 1h) but
// extends it while the attempt stays authorized: the grant, policy,
// connection, and secret version still allow it, and the attempt is still
// running (autonomous or taken over) under the lease's owner generation. A
// lease within ttl/2 of expiry is extended to now+ttl. Until acknowledged,
// the same renewal is returned without extending it. Revoked or expired leases
// are never renewed; the worker then stops at the old expiry. Pages are ordered
// by (organization, lease ID); an empty page ends the pass. Callers advance the
// cursor only over attempted deliveries, including failures, then wrap around.
func RenewModelLeases(ctx context.Context, db *pgxpool.Pool, ttl time.Duration, afterOrgID, afterLeaseID string, limit int) ([]RenewedLease, error) {
	ctx = tenant.System(ctx)
	if db == nil || ttl <= 0 || ttl > time.Hour || limit < 1 || limit > 100 || (afterOrgID == "") != (afterLeaseID == "") {
		return nil, ErrDenied
	}
	rows, err := db.Query(ctx, `WITH due AS (
		SELECT l.organization_id,l.id,g.delivery_mode
		FROM access_leases l, access_bindings b, access_grants g, access_project_policies p, access_connections c, access_secret_versions sv,
			workflow_attempts a, workflow_tasks t, workflow_runs r
		WHERE (l.organization_id,l.id)>($2,$3)
		AND l.capability='model.invoke' AND l.revoked_at IS NULL AND l.delivered_at IS NOT NULL
		AND l.expires_at>clock_timestamp() AND (l.renewal_pending OR l.expires_at<clock_timestamp()+$1::interval/2)
		AND b.organization_id=l.organization_id AND b.id=l.binding_id AND b.capability='model.invoke'
		AND g.organization_id=b.organization_id AND g.id=b.grant_id AND g.revoked_at IS NULL
		AND g.version=b.grant_version AND (g.expires_at IS NULL OR g.expires_at>=
		    CASE WHEN l.renewal_pending THEN l.expires_at ELSE clock_timestamp()+$1::interval END)
		AND p.organization_id=b.organization_id AND p.project_id=b.project_id AND p.version=b.policy_version
		AND c.organization_id=g.organization_id AND c.id=g.connection_id AND c.id=l.connection_id AND c.state='active'
		AND c.base_url=b.model_base_url
		AND sv.organization_id=c.organization_id AND sv.connection_id=c.id AND sv.version=c.active_secret_version
		AND (sv.expires_at IS NULL OR sv.expires_at>=
		    CASE WHEN l.renewal_pending THEN l.expires_at ELSE clock_timestamp()+$1::interval END)
		AND a.organization_id::text=l.organization_id AND a.id::text=l.attempt_id
		AND a.state='running' AND a.generation=l.owner_generation
		AND t.organization_id=a.organization_id AND t.id=a.task_id AND t.active_attempt_id=a.id AND t.state='running'
		AND r.organization_id=a.organization_id AND r.id=a.run_id AND r.state='active' AND r.graph_sealed
		ORDER BY l.organization_id,l.id LIMIT $4 FOR UPDATE OF l
	), renewed AS (
		UPDATE access_leases l
		SET expires_at=CASE WHEN l.renewal_pending THEN l.expires_at ELSE clock_timestamp()+$1::interval END,
		    renewal_generation=l.renewal_generation+CASE WHEN l.renewal_pending THEN 0 ELSE 1 END,
		    renewal_pending=true
		FROM due WHERE l.organization_id=due.organization_id AND l.id=due.id
		RETURNING l.organization_id,l.attempt_id,l.id,due.delivery_mode,l.expires_at,l.renewal_generation
	) SELECT * FROM renewed ORDER BY organization_id,id`, ttl, afterOrgID, afterLeaseID, limit)
	if err != nil {
		return nil, fmt.Errorf("renew model leases: %w", err)
	}
	defer rows.Close()
	var out []RenewedLease
	for rows.Next() {
		var r RenewedLease
		if err := rows.Scan(&r.OrganizationID, &r.AttemptID, &r.LeaseID, &r.DeliveryMode, &r.ExpiresAt, &r.Generation); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRenewalDelivered acknowledges only the generation actually written to
// the guest. An old acknowledgement cannot clear a newer pending renewal.
// OAuth may cap expires_at during delivery, so the fence is the generation.
func MarkRenewalDelivered(ctx context.Context, db *pgxpool.Pool, lease RenewedLease) error {
	if db == nil || lease.OrganizationID == "" || lease.LeaseID == "" || lease.AttemptID == "" || lease.Generation <= 0 {
		return ErrDenied
	}
	ctx = tenant.Org(ctx, lease.OrganizationID)
	result, err := db.Exec(ctx, `UPDATE access_leases SET renewal_pending=false
		WHERE organization_id=$1 AND id=$2 AND attempt_id=$3 AND renewal_generation=$4
		AND capability='model.invoke' AND revoked_at IS NULL AND expires_at>clock_timestamp()`,
		lease.OrganizationID, lease.LeaseID, lease.AttemptID, lease.Generation)
	if err != nil {
		return fmt.Errorf("acknowledge model lease renewal: %w", err)
	}
	if result.RowsAffected() != 1 {
		return ErrDenied
	}
	return nil
}
