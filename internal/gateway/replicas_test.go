package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// TestTwoReplicasSharePostgresState runs two gateway replicas (separate
// processes' worth of in-memory state) on one database and proves that
// admission-critical state is consistent between them: a 429 cooldown, the
// circuit breaker and its single half-open probe, concurrency caps, and
// configured per-minute quotas.
func TestTwoReplicasSharePostgresState(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := tenant.System(t.Context())
	var calls sync.Map // key -> *atomic.Int64
	count := func(key string) int64 {
		v, _ := calls.LoadOrStore(key, new(atomic.Int64))
		return v.(*atomic.Int64).Load()
	}
	hold := make(chan struct{})
	holding := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key")
		v, _ := calls.LoadOrStore(key, new(atomic.Int64))
		v.(*atomic.Int64).Add(1)
		switch key {
		case "k-limited":
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
		case "k-broken":
			w.WriteHeader(http.StatusBadGateway)
		case "k-slow":
			holding <- struct{}{}
			<-hold
			fallthrough
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"model":"m","usage":{"input_tokens":1000,"output_tokens":10}}`)
		}
	}))
	defer upstream.Close()

	route := func(id string, priority, limit int) Route {
		return Route{ID: id, Name: id, Kind: KindAnthropic, ConnectionID: id, AuthMethod: "api_key", Priority: priority, Weight: 1,
			Cap: limit, State: "enabled"}
	}
	keys := map[string]string{"r-limited": "k-limited", "r-ok": "k-ok", "r-broken": "k-broken", "r-slow": "k-slow", "r-quota": "k-quota"}
	plans := map[string]Plan{ // by leased connection
		"r-limited": {PoolID: "p1", Routes: []Route{route("r-limited", 1, 0), route("r-ok", 2, 0)}},
		"r-broken":  {Routes: []Route{route("r-broken", 1, 0)}},
		"r-slow":    {Routes: []Route{route("r-slow", 1, 1)}},
		"r-quota":   {Routes: []Route{func() Route { r := route("r-quota", 1, 0); r.RequestsPerMinute = 2; return r }()}},
	}
	replica := func() *Server {
		shared := NewShared(pool)
		shared.TTL = time.Nanosecond // always read the shared view
		return &Server{Upstream: map[string]string{"anthropic": upstream.URL}, Record: (&recorder{}).record, Shared: shared,
			Authorize: func(_ context.Context, token, _ string) (Grant, error) {
				g := testGrant("anthropic", "m")
				g.OrganizationID, g.ConnectionID, g.Key, g.AuthMethod = f.org, token[len(TokenPrefix):], []byte(keys[token[len(TokenPrefix):]]), "api_key"
				return g, nil
			},
			Plan:     func(_ context.Context, g Grant) (Plan, error) { return plans[g.ConnectionID], nil },
			RouteKey: func(_ context.Context, _ Grant, r Route) ([]byte, error) { return []byte(keys[r.ID]), nil }}
	}
	a, b := httptest.NewServer(replica()), httptest.NewServer(replica())
	defer a.Close()
	defer b.Close()
	post := func(front *httptest.Server, connection string) int {
		t.Helper()
		request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"m"}`))
		request.Header.Set("Authorization", "Bearer "+TokenPrefix+connection)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response.StatusCode
	}
	// validToken is bypassed by the stub Authorize; the server only needs a bearer.

	// 1. A 429 on replica A cools the route for replica B too.
	if code := post(a, "r-limited"); code != 200 || count("k-limited") != 1 || count("k-ok") != 1 {
		t.Fatalf("failover on A: %d limited=%d ok=%d", code, count("k-limited"), count("k-ok"))
	}
	if code := post(b, "r-limited"); code != 200 || count("k-limited") != 1 || count("k-ok") != 2 {
		t.Fatalf("B retried the cooled route: %d limited=%d", code, count("k-limited"))
	}

	// 2. Five failures across both replicas open the shared breaker.
	for i := range breakerFailures {
		post([]*httptest.Server{a, b}[i%2], "r-broken")
	}
	if count("k-broken") != breakerFailures {
		t.Fatalf("broken calls: %d", count("k-broken"))
	}
	if code := post(b, "r-broken"); code != http.StatusTooManyRequests || count("k-broken") != breakerFailures {
		t.Fatalf("open breaker still used by B: %d calls=%d", code, count("k-broken"))
	}
	var breaker string
	if err := pool.QueryRow(ctx, `SELECT breaker FROM gateway_route_state WHERE organization_id=$1 AND route_id='r-broken'`, f.org).Scan(&breaker); err != nil || breaker != BreakerOpen {
		t.Fatalf("shared breaker: %q %v", breaker, err)
	}
	// After the cooldown exactly one replica wins the half-open probe.
	if _, err := pool.Exec(ctx, `UPDATE gateway_route_state SET opened_at=clock_timestamp()-interval '31 seconds'
		WHERE organization_id=$1 AND route_id='r-broken'`, f.org); err != nil {
		t.Fatal(err)
	}
	sa, sb := NewShared(pool), NewShared(pool)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for _, s := range []*Shared{sa, sb, sa, sb} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.ClaimProbe(t.Context(), f.org, "r-broken") {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatalf("%d replicas won the half-open probe, want 1", wins.Load())
	}

	// 3. A concurrency cap of 1 holds across replicas.
	done := make(chan int)
	go func() { done <- post(a, "r-slow") }()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never reached the upstream")
	}
	if code := post(b, "r-slow"); code != http.StatusTooManyRequests || count("k-slow") != 1 {
		t.Fatalf("B passed the shared cap: %d calls=%d", code, count("k-slow"))
	}
	close(hold)
	if code := <-done; code != 200 {
		t.Fatalf("held request: %d", code)
	}
	if code := post(b, "r-slow"); code != 200 || count("k-slow") != 2 {
		t.Fatalf("slot not released: %d", code)
	}
	// A crashed replica's slots stop counting once it stops heartbeating.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_route_inflight (organization_id,slot,replica,inflight,heartbeat_at)
		VALUES ($1,'r-crashed','deadbeefdeadbeef',5,clock_timestamp()-interval '1 minute')`, f.org); err != nil {
		t.Fatal(err)
	}
	if ok, err := sa.Acquire(t.Context(), f.org, "r-crashed", 1); err != nil || !ok {
		t.Fatalf("stale slots still counted: %v %v", ok, err)
	}
	if ok, _ := sb.Acquire(t.Context(), f.org, "r-crashed", 1); ok {
		t.Fatal("live slot not counted on the other replica")
	}

	// 4. A configured quota of 2 requests a minute is shared.
	codes := []int{post(a, "r-quota"), post(b, "r-quota"), post(a, "r-quota")}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests || count("k-quota") != 2 {
		t.Fatalf("shared quota: %v calls=%d", codes, count("k-quota"))
	}
}
