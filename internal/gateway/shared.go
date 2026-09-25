package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Shared makes admission-critical route state consistent across gateway
// replicas through PostgreSQL (gateway_route_state, gateway_route_inflight):
//   - a 429's cooldown (and a reported reset) is written at once with
//     GREATEST, so every replica skips the route until it passes;
//   - the circuit breaker opens from the shared consecutive-failure count,
//     and exactly one replica wins the half-open probe (conditional update);
//   - concurrency caps count every live replica's slots under an advisory
//     lock, and a replica that stops heartbeating stops holding slots.
//
// Each replica still keeps its fast in-memory view (States) and reads the
// shared view with a short TTL before choosing routes.
type Shared struct {
	DB      *pgxpool.Pool
	Replica string        // 16 hex characters, unique per process
	TTL     time.Duration // how stale a cached shared view may be; 0 = 1 s

	mu    sync.Mutex
	cache map[string]sharedEntry
}

type sharedEntry struct {
	view SharedView
	at   time.Time
}

// SharedView is one route's admission state as all replicas see it.
type SharedView struct {
	Breaker                             string
	OpenedAt, CooldownUntil, ProbeUntil time.Time
}

// slotHeartbeat is how long a replica's slots count without a heartbeat.
const slotHeartbeat = 30 * time.Second

// NewShared returns shared state for one gateway process.
func NewShared(db *pgxpool.Pool) *Shared {
	var id [8]byte
	_, _ = rand.Read(id[:])
	return &Shared{DB: db, Replica: hex.EncodeToString(id[:]), cache: map[string]sharedEntry{}}
}

func (s *Shared) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return time.Second
}

func (s *Shared) forget(org, route string) {
	s.mu.Lock()
	delete(s.cache, org+"/"+route)
	s.mu.Unlock()
}

// Views returns the shared state of the given routes, reading those not
// cached within the TTL in one query. On a read error the cached (or empty)
// view is used: the in-memory state still protects this replica.
func (s *Shared) Views(ctx context.Context, org string, routes []string) map[string]SharedView {
	out := map[string]SharedView{}
	if s == nil || len(routes) == 0 {
		return out
	}
	now := time.Now()
	var missing []string
	s.mu.Lock()
	for _, id := range routes {
		if e, ok := s.cache[org+"/"+id]; ok && now.Sub(e.at) < s.ttl() {
			out[id] = e.view
		} else {
			missing = append(missing, id)
		}
	}
	s.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	rows, err := s.DB.Query(tenant.Org(ctx, org), `SELECT route_id,breaker,opened_at,cooldown_until,probe_until
		FROM gateway_route_state WHERE organization_id=$1 AND route_id=ANY($2)`, org, missing)
	if err != nil {
		log.Printf("gateway shared route state: %v", err)
		return out
	}
	fresh := map[string]SharedView{}
	var id, breaker string
	var opened, cooldown, probe *time.Time
	_, err = pgx.ForEachRow(rows, []any{&id, &breaker, &opened, &cooldown, &probe}, func() error {
		fresh[id] = SharedView{Breaker: breaker, OpenedAt: timeOf(opened), CooldownUntil: timeOf(cooldown), ProbeUntil: timeOf(probe)}
		return nil
	})
	if err != nil {
		log.Printf("gateway shared route state: %v", err)
		return out
	}
	s.mu.Lock()
	if len(s.cache) > 100_000 {
		s.cache = map[string]sharedEntry{}
	}
	for _, id := range missing {
		view := fresh[id] // absent rows are a closed breaker with no cooldown.
		s.cache[org+"/"+id] = sharedEntry{view: view, at: now}
		out[id] = view
	}
	s.mu.Unlock()
	return out
}

func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// availableAt is when the shared view lets a route take a request (zero:
// now). An open breaker past its cooldown is available to the one replica
// that claims the probe.
func (v SharedView) availableAt(now time.Time) time.Time {
	var at time.Time
	later := func(t time.Time) {
		if t.After(now) && (at.IsZero() || t.Before(at)) {
			at = t
		}
	}
	later(v.CooldownUntil)
	if v.needsProbe() {
		later(v.OpenedAt.Add(breakerCooldown))
		if !v.OpenedAt.Add(breakerCooldown).After(now) {
			later(v.ProbeUntil)
		}
	}
	return at
}

// needsProbe reports whether the route may only be used as the breaker's
// single half-open probe.
func (v SharedView) needsProbe() bool {
	return v.Breaker == BreakerOpen || v.Breaker == BreakerHalfOpen
}

// ClaimProbe lets exactly one replica send the half-open probe of a route
// whose breaker cooled down.
func (s *Shared) ClaimProbe(ctx context.Context, org, route string) bool {
	defer s.forget(org, route)
	tag, err := s.DB.Exec(tenant.Org(ctx, org), `UPDATE gateway_route_state
		SET probe_until=clock_timestamp()+$4::interval, updated_at=clock_timestamp()
		WHERE organization_id=$1 AND route_id=$2 AND breaker IN ('open','half_open') AND opened_at<=clock_timestamp()-$3::interval
		AND (probe_until IS NULL OR probe_until<clock_timestamp())`, org, route, breakerCooldown, breakerCooldown)
	return err == nil && tag.RowsAffected() == 1
}

// Acquire takes one concurrency slot (a route id, or "pool:<id>") if fewer
// than limit are held by live replicas. The caller releases it.
func (s *Shared) Acquire(ctx context.Context, org, slot string, limit int) (bool, error) {
	if limit <= 0 {
		return true, nil
	}
	ctx = tenant.Org(ctx, org)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('gateway-slot/'||$1||'/'||$2, 0))`, org, slot); err != nil {
		return false, err
	}
	var held int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(inflight),0) FROM gateway_route_inflight
		WHERE organization_id=$1 AND slot=$2 AND (replica=$3 OR heartbeat_at>clock_timestamp()-$4::interval)`,
		org, slot, s.Replica, slotHeartbeat).Scan(&held); err != nil {
		return false, err
	}
	if held >= limit {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gateway_route_inflight AS i (organization_id,slot,replica,inflight,heartbeat_at)
		VALUES ($1,$2,$3,1,clock_timestamp())
		ON CONFLICT (organization_id,slot,replica) DO UPDATE SET inflight=i.inflight+1, heartbeat_at=clock_timestamp()`,
		org, slot, s.Replica); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Count adds add to a per-minute quota counter shared by all replicas
// (slot "rpm:<route>:<minute>" or "tpm:<route>:<minute>") unless that would
// pass limit (0: no limit). It reports whether the addition was made.
func (s *Shared) Count(ctx context.Context, org, slot string, add int64, limit int64) (bool, error) {
	ctx = tenant.Org(context.WithoutCancel(ctx), org)
	tx, err := s.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('gateway-slot/'||$1||'/'||$2, 0))`, org, slot); err != nil {
		return false, err
	}
	var used int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(inflight),0) FROM gateway_route_inflight WHERE organization_id=$1 AND slot=$2`,
		org, slot).Scan(&used); err != nil {
		return false, err
	}
	if limit > 0 && used+max(add, 1) > limit {
		return false, nil
	}
	if add > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO gateway_route_inflight AS i (organization_id,slot,replica,inflight,heartbeat_at)
			VALUES ($1,$2,$3,LEAST($4,2000000000),clock_timestamp())
			ON CONFLICT (organization_id,slot,replica) DO UPDATE SET inflight=LEAST(i.inflight::bigint+$4,2000000000), heartbeat_at=clock_timestamp()`,
			org, slot, s.Replica, add); err != nil {
			return false, err
		}
	}
	return true, tx.Commit(ctx)
}

func minuteSlot(kind, route string, now time.Time) string {
	return kind + ":" + route + ":" + now.UTC().Format("200601021504")
}

// Release returns a slot taken with Acquire.
func (s *Shared) Release(ctx context.Context, org, slot string) {
	if _, err := s.DB.Exec(tenant.Org(context.WithoutCancel(ctx), org), `UPDATE gateway_route_inflight
		SET inflight=GREATEST(inflight-1,0), heartbeat_at=clock_timestamp()
		WHERE organization_id=$1 AND slot=$2 AND replica=$3`, org, slot, s.Replica); err != nil {
		log.Printf("gateway release slot: %v", err)
	}
}

// KeepAlive heartbeats every interval until ctx ends.
func (s *Shared) KeepAlive(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := s.Heartbeat(ctx); err != nil && ctx.Err() == nil {
			log.Printf("gateway slot heartbeat: %v", err)
		}
	}
}

// Heartbeat keeps this replica's held slots counted, and drops empty rows.
func (s *Shared) Heartbeat(ctx context.Context) error {
	ctx = tenant.System(ctx) // one replica holds slots in every organization it serves.
	if _, err := s.DB.Exec(ctx, `UPDATE gateway_route_inflight SET heartbeat_at=clock_timestamp()
		WHERE replica=$1 AND inflight>0`, s.Replica); err != nil {
		return err
	}
	_, err := s.DB.Exec(ctx, `DELETE FROM gateway_route_inflight
		WHERE (inflight=0 AND heartbeat_at<clock_timestamp()-$1::interval)
		OR ((slot LIKE 'rpm:%' OR slot LIKE 'tpm:%') AND heartbeat_at<clock_timestamp()-interval '2 minutes')`, slotHeartbeat)
	return err
}

// Observe records one request's outcome in the shared state: a cooldown
// until retry-after or a reported reset (never shortened), a failure that
// may open the breaker (from the shared consecutive count, or because this
// replica's error window opened it), or a success that closes it.
func (s *Shared) Observe(ctx context.Context, org, route string, status int, cooldownUntil time.Time, openLocally bool) error {
	defer s.forget(org, route)
	ctx = tenant.Org(context.WithoutCancel(ctx), org)
	failed := status == 0 || status == 408 || status >= 500
	if !cooldownUntil.IsZero() {
		if _, err := s.DB.Exec(ctx, `INSERT INTO gateway_route_state AS s (organization_id,route_id,cooldown_until,last_status,last_429_at)
			VALUES ($1,$2,$3,$4,CASE WHEN $4=429 THEN clock_timestamp() END)
			ON CONFLICT (organization_id,route_id) DO UPDATE SET cooldown_until=GREATEST(s.cooldown_until,EXCLUDED.cooldown_until),
			last_status=EXCLUDED.last_status, last_429_at=COALESCE(EXCLUDED.last_429_at,s.last_429_at), updated_at=clock_timestamp()`,
			org, route, cooldownUntil, status); err != nil {
			return err
		}
	}
	switch {
	case failed:
		_, err := s.DB.Exec(ctx, `INSERT INTO gateway_route_state AS s
			(organization_id,route_id,consecutive_failures,last_status,breaker,opened_at)
			VALUES ($1,$2,1,$3,CASE WHEN $4 THEN 'open' ELSE 'closed' END,CASE WHEN $4 THEN clock_timestamp() END)
			ON CONFLICT (organization_id,route_id) DO UPDATE SET
			consecutive_failures=s.consecutive_failures+1, last_status=EXCLUDED.last_status,
			breaker=CASE WHEN $4 OR s.consecutive_failures+1>=$5 OR s.breaker IN ('open','half_open') THEN 'open' ELSE 'closed' END,
			opened_at=CASE WHEN $4 OR s.consecutive_failures+1>=$5 OR s.breaker IN ('open','half_open') THEN clock_timestamp() ELSE s.opened_at END,
			probe_until=NULL, updated_at=clock_timestamp()`, org, route, status, openLocally, breakerFailures)
		return err
	case status > 0 && status < 400:
		_, err := s.DB.Exec(ctx, `UPDATE gateway_route_state SET consecutive_failures=0, breaker='closed', opened_at=NULL,
			probe_until=NULL, last_status=$3, updated_at=clock_timestamp()
			WHERE organization_id=$1 AND route_id=$2 AND (consecutive_failures<>0 OR breaker<>'closed' OR probe_until IS NOT NULL)`,
			org, route, status)
		return err
	}
	return nil
}

// used adds a finished request's tokens to its route's shared per-minute count.
func (s *Shared) used(ctx context.Context, org string, route Route, tokens int64) {
	if s == nil || route.TokensPerMinute <= 0 || tokens <= 0 {
		return
	}
	if _, err := s.Count(ctx, org, minuteSlot("tpm", route.ID, time.Now()), tokens, 0); err != nil {
		log.Printf("gateway token quota: %v", err)
	}
}

// errNoSlot marks a route skipped for want of a concurrency slot or probe.
var errNoSlot = errors.New("route has no free slot")

// admit takes what a request on route needs across replicas: the half-open
// probe if the shared breaker is open, and route and pool concurrency
// slots. It returns the release function, or errNoSlot to try the next route.
func (s *Shared) admit(ctx context.Context, org string, plan Plan, route Route, view SharedView) (func(), error) {
	if s == nil {
		return func() {}, nil
	}
	if view.needsProbe() && !s.ClaimProbe(ctx, org, route.ID) {
		return nil, errNoSlot
	}
	// Configured quotas (Bedrock, Vertex) are counted per minute across
	// replicas: a request takes one of the minute's requests, and the
	// minute's tokens so far must be under the limit.
	now := time.Now()
	if route.TokensPerMinute > 0 {
		if ok, err := s.Count(ctx, org, minuteSlot("tpm", route.ID, now), 0, route.TokensPerMinute); err != nil || !ok {
			return nil, errNoSlot
		}
	}
	if route.RequestsPerMinute > 0 {
		if ok, err := s.Count(ctx, org, minuteSlot("rpm", route.ID, now), 1, int64(route.RequestsPerMinute)); err != nil || !ok {
			return nil, errNoSlot
		}
	}
	var held []string
	release := func() {
		for _, slot := range held {
			s.Release(ctx, org, slot)
		}
	}
	for _, slot := range []struct {
		name  string
		limit int
	}{{route.ID, route.Cap}, {"pool:" + plan.PoolID, plan.PoolCap}} {
		if slot.limit <= 0 || slot.name == "pool:" {
			continue
		}
		ok, err := s.Acquire(ctx, org, slot.name, slot.limit)
		if err != nil || !ok {
			release()
			if err != nil {
				log.Printf("gateway slot: %v", err)
			}
			return nil, errNoSlot
		}
		held = append(held, slot.name)
	}
	return release, nil
}
