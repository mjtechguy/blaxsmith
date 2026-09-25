package gateway

import (
	"context"
	"encoding/json"
	"log"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Circuit breaker tuning (§4): a route opens after breakerFailures
// consecutive failures, or when at least breakerMinSamples results in the
// last breakerWindow are half failures. It cools down for breakerCooldown,
// then lets one probe through (half-open); a success closes it.
const (
	breakerFailures   = 5
	breakerMinSamples = 10
	breakerWindow     = time.Minute
	breakerCooldown   = 30 * time.Second
	// A 429 without retry-after cools the route for this long.
	defaultRateCooldown = 5 * time.Second
	maxRateCooldown     = 5 * time.Minute
	// Failover (§4): the first route plus at most two more.
	maxRouteTries = 3
	errorWindow   = 15 * time.Minute
)

const (
	BreakerClosed   = "closed"
	BreakerOpen     = "open"
	BreakerHalfOpen = "half_open"
)

type result struct {
	at     time.Time
	failed bool
}

// routeState is one route's live state on this replica (§5).
type routeState struct {
	org           string
	inflight      int
	breaker       string
	openedAt      time.Time
	probing       bool
	cooldownUntil time.Time
	consecutive   int
	results       []result // newest last, bounded
	metrics       map[string]Metric
	// Configured-quota window for routes without rate headers (Bedrock, Vertex).
	windowStart                  time.Time
	windowRequests, windowTokens int64
	lastStatus                   int
	last429                      time.Time
	dirty                        bool
}

// States is this replica's fast in-memory route state: rate-limit metrics,
// its own breaker window, concurrency and prompt-cache affinity. Metrics and
// 15-minute counts are persisted to gateway_route_state for the UI and the
// dispatcher. Admission-critical state (cooldowns, the breaker, concurrency
// and configured quotas) is also shared across replicas through Shared,
// whose view Order takes as input.
type States struct {
	mu       sync.Mutex
	routes   map[string]*routeState // org/route
	affinity map[string]string      // attempt -> route id
	now      func() time.Time
}

func NewStates() *States {
	return &States{routes: map[string]*routeState{}, affinity: map[string]string{}, now: time.Now}
}

func (s *States) get(org, route string) *routeState {
	key := org + "/" + route
	st := s.routes[key]
	if st == nil {
		st = &routeState{org: org, breaker: BreakerClosed, metrics: map[string]Metric{}}
		s.routes[key] = st
	}
	return st
}

// availableAt reports when the route can take a request: zero when now.
func (st *routeState) availableAt(r Route, estimate int64, now time.Time) time.Time {
	var at time.Time
	later := func(t time.Time) {
		if t.After(now) && (at.IsZero() || t.Before(at)) {
			at = t
		}
	}
	if st.breaker == BreakerOpen {
		if now.Sub(st.openedAt) < breakerCooldown {
			later(st.openedAt.Add(breakerCooldown))
		}
	} else if st.breaker == BreakerHalfOpen && st.probing {
		later(now.Add(time.Second))
	}
	if st.cooldownUntil.After(now) {
		later(st.cooldownUntil)
	}
	for _, m := range st.metrics {
		if m.exhausted(estimate, now) {
			later(m.ResetAt)
		}
	}
	if r.RequestsPerMinute > 0 || r.TokensPerMinute > 0 {
		if now.Sub(st.windowStart) < time.Minute &&
			(r.RequestsPerMinute > 0 && st.windowRequests >= int64(r.RequestsPerMinute) ||
				r.TokensPerMinute > 0 && st.windowTokens+estimate > r.TokensPerMinute) {
			later(st.windowStart.Add(time.Minute))
		}
	}
	if r.Cap > 0 && st.inflight >= r.Cap {
		later(now.Add(time.Second)) // a slot frees as soon as a request ends.
	}
	return at
}

// headroom is the smallest remaining fraction of any known metric (1 when
// the provider reports none), used to prefer the route with the most quota.
func (st *routeState) headroom(now time.Time) float64 {
	h := 1.0
	for _, m := range st.metrics {
		if m.Limit > 0 && m.ResetAt.After(now) {
			h = min(h, float64(m.Remaining)/float64(m.Limit))
		}
	}
	return h
}

// Order returns the routes to try for one request, best first, at most
// maxRouteTries of them, or when the earliest route frees up if none can
// take it now. Unhealthy, cooling-down, over-cap, exhausted and
// model- or endpoint-unsupported routes are filtered out (§4).
func (s *States) Order(org string, plan Plan, model, attempt, path string, shared map[string]SharedView) ([]Route, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var ready, draining []Route
	var wait time.Time
	poolInflight := 0
	for _, r := range plan.Routes {
		poolInflight += s.get(org, r.ID).inflight
	}
	if plan.PoolCap > 0 && poolInflight >= plan.PoolCap {
		return nil, now.Add(time.Second)
	}
	headroom := map[string]float64{}
	for _, r := range plan.Routes {
		if r.State == "disabled" || !r.serves(path) {
			continue
		}
		if _, ok := r.model(model); !ok {
			continue
		}
		st := s.get(org, r.ID)
		at := st.availableAt(r, 1, now)
		if v, ok := shared[r.ID]; ok {
			if vat := v.availableAt(now); vat.After(at) {
				at = vat // both this replica's view and the shared one must allow it.
			}
		}
		if !at.IsZero() {
			if wait.IsZero() || at.Before(wait) {
				wait = at
			}
			continue
		}
		headroom[r.ID] = st.headroom(now)
		if r.State == "draining" {
			draining = append(draining, r)
		} else {
			ready = append(ready, r)
		}
	}
	switch plan.Strategy {
	case StrategyWeighted:
		// Weighted random order without replacement (Efraimidis–Spirakis).
		keys := map[string]float64{}
		for _, r := range ready {
			keys[r.ID] = -rand.ExpFloat64() / float64(max(r.Weight, 1))
		}
		sort.SliceStable(ready, func(i, j int) bool { return keys[ready[i].ID] > keys[ready[j].ID] })
	case StrategyFillFirst:
		sort.SliceStable(ready, func(i, j int) bool { return ready[i].Priority < ready[j].Priority })
	default:
		sort.SliceStable(ready, func(i, j int) bool {
			if ready[i].Priority != ready[j].Priority {
				return ready[i].Priority < ready[j].Priority
			}
			return headroom[ready[i].ID] > headroom[ready[j].ID]
		})
	}
	// Prompt-cache affinity: the route this attempt last used goes first
	// while it is healthy, so provider-side caches stay warm.
	if last := s.affinity[attempt]; plan.Affinity && last != "" {
		for i, r := range ready {
			if r.ID == last {
				copy(ready[1:i+1], ready[:i])
				ready[0] = r
				break
			}
		}
	}
	out := append(ready, draining...)
	if len(out) > maxRouteTries {
		out = out[:maxRouteTries]
	}
	// No routes and no wait: nothing here serves this model or endpoint.
	return out, wait
}

// Outcome is what one finished request means for the shared state.
type Outcome struct {
	CooldownUntil time.Time // a 429's retry-after, or an exhausted metric's reset
	Opened        bool      // this replica's breaker opened
}

// Begin counts a request against the route and returns the function that
// records its outcome: the HTTP status (0 for a transport failure), the
// response headers, and the tokens it used.
func (s *States) Begin(org string, r Route, attempt string) func(status int, h http.Header, tokens int64) Outcome {
	s.mu.Lock()
	st := s.get(org, r.ID)
	st.inflight++
	if st.breaker == BreakerOpen && s.now().Sub(st.openedAt) >= breakerCooldown {
		st.breaker = BreakerHalfOpen
	}
	if st.breaker == BreakerHalfOpen {
		st.probing = true
	}
	st.dirty = true
	s.mu.Unlock()
	var once sync.Once
	var out Outcome
	return func(status int, h http.Header, tokens int64) Outcome {
		once.Do(func() { out = s.finish(org, r, attempt, st, status, h, tokens) })
		return out
	}
}

func (s *States) finish(org string, r Route, attempt string, st *routeState, status int, h http.Header, tokens int64) (out Outcome) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	st.inflight = max(st.inflight-1, 0)
	st.lastStatus, st.dirty = status, true
	if r.RequestsPerMinute > 0 || r.TokensPerMinute > 0 {
		if now.Sub(st.windowStart) >= time.Minute {
			st.windowStart, st.windowRequests, st.windowTokens = now, 0, 0
		}
		st.windowRequests++
		st.windowTokens += tokens
	}
	if h != nil {
		metrics, wait := rateHeaders(h, now)
		for name, m := range metrics {
			st.metrics[name] = m
		}
		if status == http.StatusTooManyRequests || wait > 0 && status >= 500 {
			if wait <= 0 {
				wait = defaultRateCooldown
			}
			st.cooldownUntil = now.Add(min(wait, maxRateCooldown))
		}
	}
	if status == http.StatusTooManyRequests {
		st.last429 = now
		if h == nil {
			st.cooldownUntil = now.Add(defaultRateCooldown)
		}
	}
	if st.cooldownUntil.After(now) {
		out.CooldownUntil = st.cooldownUntil
	}
	for _, m := range st.metrics {
		if m.exhausted(1, now) && m.ResetAt.After(out.CooldownUntil) {
			out.CooldownUntil = m.ResetAt // every replica waits for a known reset.
		}
	}
	// 429 is quota, not ill health: it cools the route but never trips the breaker.
	failed := status == 0 || status == http.StatusRequestTimeout || status >= 500
	st.results = append(st.results, result{at: now, failed: failed || status == http.StatusTooManyRequests})
	if len(st.results) > 2000 {
		st.results = append([]result(nil), st.results[len(st.results)-1000:]...)
	}
	st.probing = false
	if failed {
		st.consecutive++
		if st.breaker == BreakerHalfOpen || st.consecutive >= breakerFailures || windowFailing(st.results, now) {
			st.breaker, st.openedAt = BreakerOpen, now
			out.Opened = true
		}
		return out
	}
	st.consecutive = 0
	if status < 400 {
		st.breaker = BreakerClosed
		if attempt != "" {
			if len(s.affinity) > 100_000 {
				s.affinity = map[string]string{}
			}
			s.affinity[attempt] = r.ID
		}
	}
	return out
}

func windowFailing(results []result, now time.Time) bool {
	var n, failed int
	for i := len(results) - 1; i >= 0 && now.Sub(results[i].at) <= breakerWindow; i-- {
		n++
		if results[i].failed {
			failed++
		}
	}
	return n >= breakerMinSamples && failed*2 >= n
}

// RouteView is one route's state as persisted and shown.
type RouteView struct {
	Breaker       string
	CooldownUntil time.Time
	Inflight      int
	Metrics       map[string]Metric
	Requests15m   int
	Errors15m     int
	LastStatus    int
	Last429       time.Time
}

// View returns the route's current state on this replica.
func (s *States) View(org, route string) RouteView {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.view(s.get(org, route), s.now())
}

func (s *States) view(st *routeState, now time.Time) RouteView {
	v := RouteView{Breaker: st.breaker, CooldownUntil: st.cooldownUntil, Inflight: st.inflight,
		Metrics: map[string]Metric{}, LastStatus: st.lastStatus, Last429: st.last429}
	if v.Breaker == BreakerOpen && now.Sub(st.openedAt) >= breakerCooldown {
		v.Breaker = BreakerHalfOpen
	}
	if v.Breaker == BreakerOpen {
		v.CooldownUntil = maxTime(v.CooldownUntil, st.openedAt.Add(breakerCooldown))
	}
	for k, m := range st.metrics {
		v.Metrics[k] = m
	}
	for i := len(st.results) - 1; i >= 0 && now.Sub(st.results[i].at) <= errorWindow; i-- {
		v.Requests15m++
		if st.results[i].failed {
			v.Errors15m++
		}
	}
	return v
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// Persist writes changed route state every interval until ctx ends (§5:
// about every 5 seconds).
func (s *States) Persist(ctx context.Context, db *pgxpool.Pool, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := s.Flush(ctx, db); err != nil && ctx.Err() == nil {
			log.Printf("gateway route state: %v", err)
		}
	}
}

// Flush writes every changed route's state once.
func (s *States) Flush(ctx context.Context, db *pgxpool.Pool) error {
	ctx = tenant.System(ctx) // one replica's state spans every organization it served; each row names its own.
	type row struct {
		org, route string
		v          RouteView
	}
	s.mu.Lock()
	now := s.now()
	var rows []row
	for key, st := range s.routes {
		if !st.dirty {
			continue
		}
		st.dirty = st.inflight > 0 // keep refreshing while requests are open.
		rows = append(rows, row{org: st.org, route: key[len(st.org)+1:], v: s.view(st, now)})
	}
	s.mu.Unlock()
	for _, r := range rows {
		metrics, _ := json.Marshal(r.v.Metrics)
		if _, err := db.Exec(ctx, `INSERT INTO gateway_route_state AS s
			(organization_id,route_id,breaker,cooldown_until,inflight,metrics,requests_15m,errors_15m,last_status,last_429_at,updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,clock_timestamp())
			ON CONFLICT (organization_id,route_id) DO UPDATE SET inflight=EXCLUDED.inflight, metrics=EXCLUDED.metrics,
			requests_15m=EXCLUDED.requests_15m, errors_15m=EXCLUDED.errors_15m, last_status=EXCLUDED.last_status,
			last_429_at=COALESCE(EXCLUDED.last_429_at,s.last_429_at), updated_at=EXCLUDED.updated_at`,
			r.org, r.route, r.v.Breaker, nullTime(r.v.CooldownUntil), r.v.Inflight, metrics, r.v.Requests15m,
			r.v.Errors15m, r.v.LastStatus, nullTime(r.v.Last429)); err != nil {
			return err
		}
	}
	return nil
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
