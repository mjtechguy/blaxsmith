package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// RecordSubscriptionLimits stores the windows a personal route reported, for
// its owner only (§6, §9.2). Display only: nothing is paced on them.
func RecordSubscriptionLimits(ctx context.Context, db *pgxpool.Pool, g Grant, windows []Window) error {
	if db == nil || g.OwnerID == "" || g.ConnectionID == "" {
		return ErrDenied
	}
	ctx = tenant.Org(ctx, g.OrganizationID)
	for _, w := range windows {
		var minutes *int
		if w.WindowMinutes > 0 {
			minutes = &w.WindowMinutes
		}
		if _, err := db.Exec(ctx, `INSERT INTO gateway_subscription_limits
			(organization_id,connection_id,principal_id,window_name,used_pct,window_minutes,resets_at,observed_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,clock_timestamp())
			ON CONFLICT (organization_id,connection_id,window_name) DO UPDATE SET principal_id=EXCLUDED.principal_id,
			used_pct=EXCLUDED.used_pct, window_minutes=EXCLUDED.window_minutes, resets_at=EXCLUDED.resets_at,
			observed_at=EXCLUDED.observed_at`, g.OrganizationID, g.ConnectionID, g.OwnerID, w.Name, w.UsedPct, minutes,
			nullTime(w.ResetsAt)); err != nil {
			return fmt.Errorf("record subscription limits: %w", err)
		}
	}
	return nil
}

// Headroom is a pool's capacity for new work as the gateway last reported it.
type Headroom struct {
	OK      bool
	ResetAt time.Time // when the earliest route frees up; zero when unknown
	Reason  string
}

// HeadroomFor answers the dispatcher (§5, §11): can this pool take a stage
// that needs about estimate tokens now? It reads the persisted route state,
// so it works from the app process and across gateway replicas.
func HeadroomFor(ctx context.Context, db *pgxpool.Pool, orgID, poolID string, estimate int64) (Headroom, error) {
	ctx = tenant.Org(ctx, orgID)
	var name string
	var poolCap int
	if err := db.QueryRow(ctx, `SELECT name,concurrency_cap FROM gateway_pools WHERE organization_id=$1 AND id=$2`,
		orgID, poolID).Scan(&name, &poolCap); err != nil {
		return Headroom{}, fmt.Errorf("gateway pool: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT r.state,r.concurrency_cap,COALESCE(s.breaker,'closed'),s.cooldown_until,
		COALESCE(s.inflight,0),COALESCE(s.metrics,'{}'),s.updated_at
		FROM gateway_pool_routes pr
		JOIN gateway_routes r ON r.organization_id=pr.organization_id AND r.id=pr.route_id
		LEFT JOIN gateway_route_state s ON s.organization_id=r.organization_id AND s.route_id=r.id::text
		WHERE pr.organization_id=$1 AND pr.pool_id=$2 AND r.state<>'disabled'`, orgID, poolID)
	if err != nil {
		return Headroom{}, err
	}
	defer rows.Close()
	now := time.Now()
	var earliest time.Time
	inflight, routes := 0, 0
	ok := false
	for rows.Next() {
		var state, breaker string
		var routeCap, busy int
		var cooldown, updated *time.Time
		var raw []byte
		if err := rows.Scan(&state, &routeCap, &breaker, &cooldown, &busy, &raw, &updated); err != nil {
			return Headroom{}, err
		}
		routes++
		// State older than a minute is stale (no traffic, or the gateway is
		// down): only its future reset and cooldown times still count.
		if updated != nil && now.Sub(*updated) > time.Minute {
			busy = 0
			if breaker == BreakerOpen {
				breaker = BreakerHalfOpen
			}
		}
		inflight += busy
		var metrics map[string]Metric
		_ = json.Unmarshal(raw, &metrics)
		var at time.Time
		later := func(t time.Time) {
			if t.After(now) && (at.IsZero() || t.Before(at)) {
				at = t
			}
		}
		if breaker == BreakerOpen {
			later(now.Add(breakerCooldown))
		}
		if cooldown != nil {
			later(*cooldown)
		}
		for _, m := range metrics {
			if m.exhausted(estimate, now) {
				later(m.ResetAt)
			}
		}
		if routeCap > 0 && busy >= routeCap {
			later(now.Add(5 * time.Second))
		}
		if at.IsZero() {
			ok = true
		} else if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	if err := rows.Err(); err != nil {
		return Headroom{}, err
	}
	switch {
	case routes == 0:
		return Headroom{OK: true}, nil // an empty pool falls back to the leased connection.
	case poolCap > 0 && inflight >= poolCap:
		return Headroom{ResetAt: now.Add(5 * time.Second), Reason: "Waiting for " + name + ": all concurrency slots are busy"}, nil
	case ok:
		return Headroom{OK: true}, nil
	}
	return Headroom{ResetAt: earliest, Reason: "Waiting for " + name + " headroom"}, nil
}

// PacedError is returned to the dispatcher when a stage stays queued for
// headroom. The task is left ready; dispatch retries it after RetryAt.
type PacedError struct {
	PoolID, Reason   string
	ResetAt, RetryAt time.Time
}

func (e *PacedError) Error() string { return e.Reason }

// ErrPaced matches any *PacedError with errors.Is.
var ErrPaced = errors.New("stage paced for gateway headroom")

func (e *PacedError) Is(target error) bool { return target == ErrPaced }

// PaceRequest names one ready stage about to be dispatched in gateway mode.
type PaceRequest struct {
	OrganizationID, ProjectID, RunID, TaskID string
	Family, ConnectionID                     string
	EstimateTokens                           int64
}

// Pace is the dispatcher's back-pressure check (§5). With Rate-aware
// pacing on and the stage's pool out of headroom, it records the visible
// reason and reset time and returns a *PacedError; otherwise it clears any
// earlier pacing and returns nil. Personal subscriptions are never pooled
// and so never paced here.
func Pace(ctx context.Context, db *pgxpool.Pool, request PaceRequest) error {
	ctx = tenant.Org(ctx, request.OrganizationID)
	var poolID, name, strategy string
	var poolCap int
	var affinity bool
	err := db.QueryRow(ctx, poolForConnection+` AND s.pacing_enabled ORDER BY p.name LIMIT 1`, request.OrganizationID,
		request.Family, request.ConnectionID, request.ProjectID).Scan(&poolID, &name, &strategy, &poolCap, &affinity)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClearPaced(ctx, db, request.OrganizationID, request.TaskID)
	}
	if err != nil {
		return fmt.Errorf("gateway pacing pool: %w", err)
	}
	headroom, err := HeadroomFor(ctx, db, request.OrganizationID, poolID, max(request.EstimateTokens, 1))
	if err != nil {
		return err
	}
	if headroom.OK {
		return ClearPaced(ctx, db, request.OrganizationID, request.TaskID)
	}
	now := time.Now()
	// Re-check at the reset, but at least every 30 s and at most every 2 s.
	retry := now.Add(30 * time.Second)
	if !headroom.ResetAt.IsZero() && headroom.ResetAt.Before(retry) {
		retry = headroom.ResetAt
	}
	retry = maxTime(retry, now.Add(2*time.Second))
	if _, err := db.Exec(ctx, `INSERT INTO gateway_paced_tasks (organization_id,task_id,run_id,pool_id,reason,resets_at,retry_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (organization_id,task_id) DO UPDATE SET pool_id=EXCLUDED.pool_id, reason=EXCLUDED.reason,
		resets_at=EXCLUDED.resets_at, retry_at=EXCLUDED.retry_at, updated_at=clock_timestamp()`,
		request.OrganizationID, request.TaskID, request.RunID, poolID, headroom.Reason, nullTime(headroom.ResetAt), retry); err != nil {
		return fmt.Errorf("record paced stage: %w", err)
	}
	return &PacedError{PoolID: poolID, Reason: headroom.Reason, ResetAt: headroom.ResetAt, RetryAt: retry}
}

// ClearPaced removes a stage's pacing once it can be (or has been) dispatched.
func ClearPaced(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, orgID, taskID string) error {
	ctx = tenant.Org(ctx, orgID)
	_, err := db.Exec(ctx, `DELETE FROM gateway_paced_tasks WHERE organization_id=$1 AND task_id=$2`, orgID, taskID)
	return err
}
