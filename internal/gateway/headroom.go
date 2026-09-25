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
	var poolCap, inflight int
	// Concurrency is every live replica's held slots (gateway_route_inflight).
	if err := db.QueryRow(ctx, `SELECT p.name,p.concurrency_cap,COALESCE((SELECT sum(i.inflight) FROM gateway_route_inflight i
			WHERE i.organization_id=p.organization_id AND i.slot='pool:'||p.id::text AND i.heartbeat_at>clock_timestamp()-$3::interval),0)
		FROM gateway_pools p WHERE p.organization_id=$1 AND p.id=$2`,
		orgID, poolID, slotHeartbeat).Scan(&name, &poolCap, &inflight); err != nil {
		return Headroom{}, fmt.Errorf("gateway pool: %w", err)
	}
	rows, err := db.Query(ctx, `SELECT r.state,r.concurrency_cap,COALESCE(s.breaker,'closed'),s.opened_at,s.cooldown_until,
		COALESCE((SELECT sum(i.inflight) FROM gateway_route_inflight i WHERE i.organization_id=r.organization_id
			AND i.slot=r.id::text AND i.heartbeat_at>clock_timestamp()-$3::interval),0),COALESCE(s.metrics,'{}')
		FROM gateway_pool_routes pr
		JOIN gateway_routes r ON r.organization_id=pr.organization_id AND r.id=pr.route_id
		LEFT JOIN gateway_route_state s ON s.organization_id=r.organization_id AND s.route_id=r.id::text
		WHERE pr.organization_id=$1 AND pr.pool_id=$2 AND r.state<>'disabled'`, orgID, poolID, slotHeartbeat)
	if err != nil {
		return Headroom{}, err
	}
	defer rows.Close()
	now := time.Now()
	var earliest time.Time
	routes := 0
	ok := false
	for rows.Next() {
		var state, breaker string
		var routeCap, busy int
		var opened, cooldown *time.Time
		var raw []byte
		if err := rows.Scan(&state, &routeCap, &breaker, &opened, &cooldown, &busy, &raw); err != nil {
			return Headroom{}, err
		}
		routes++
		var metrics map[string]Metric
		_ = json.Unmarshal(raw, &metrics)
		var at time.Time
		later := func(t time.Time) {
			if t.After(now) && (at.IsZero() || t.Before(at)) {
				at = t
			}
		}
		// The shared view decides, as the gateway's admission does.
		later((SharedView{Breaker: breaker, OpenedAt: timeOf(opened), CooldownUntil: timeOf(cooldown)}).availableAt(now))
		for name, m := range metrics {
			if m.exhausted(metricNeed(name, estimate), now) {
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
	StageKey, Family, ConnectionID           string
	EstimateTokens                           int64 // 0: EstimateTokens decides
}

// defaultRequestTokens is a conservative first-request estimate per model
// family, used until a stage has its own history: an agent's first model
// call already carries its system prompt, tools and the repository context.
var defaultRequestTokens = map[string]int64{"anthropic": 48_000, "openai": 48_000, "opencode": 32_000, "opencode-go": 32_000}

// EstimateTokens is the tokens one model request of this stage is likely to
// use: the average of the same stage's recent successful requests in the
// project (at least 3 in the last 7 days), else the family default.
func EstimateTokens(ctx context.Context, db *pgxpool.Pool, orgID, projectID, stageKey, family string) (int64, error) {
	ctx = tenant.Org(ctx, orgID)
	var average, samples int64
	if err := db.QueryRow(ctx, `SELECT COALESCE(avg(total),0)::bigint,count(*) FROM (
		SELECT input_tokens+output_tokens+cache_read_tokens+cache_write_tokens AS total FROM gateway_usage_events
		WHERE organization_id=$1 AND project_id=$2 AND stage_key=$3 AND status='ok' AND usage_reported
		AND started_at>clock_timestamp()-interval '7 days' ORDER BY started_at DESC LIMIT 200) recent`,
		orgID, projectID, stageKey).Scan(&average, &samples); err != nil {
		return 0, fmt.Errorf("estimate stage tokens: %w", err)
	}
	if samples >= 3 && average > 0 {
		return average, nil
	}
	if d, ok := defaultRequestTokens[family]; ok {
		return d, nil
	}
	return 32_000, nil
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
	estimate := request.EstimateTokens
	if estimate <= 0 {
		if estimate, err = EstimateTokens(ctx, db, request.OrganizationID, request.ProjectID, request.StageKey, request.Family); err != nil {
			return err
		}
	}
	headroom, err := HeadroomFor(ctx, db, request.OrganizationID, poolID, estimate)
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
