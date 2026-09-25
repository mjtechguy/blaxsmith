package gateway

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

// twoRoutePlan is a pool of two Anthropic API-key routes: a (priority 1)
// and b (priority 2).
func twoRoutePlan() Plan {
	return Plan{PoolID: "pool-1", PoolName: "Claude production", Strategy: StrategyPriorityHeadroom, Affinity: true,
		Routes: []Route{
			{ID: "route-a", Name: "a", Kind: KindAnthropic, ConnectionID: "connection-1", AuthMethod: "api_key", Priority: 1, Weight: 1, State: "enabled"},
			{ID: "route-b", Name: "b", Kind: KindAnthropic, ConnectionID: "connection-2", AuthMethod: "api_key", Priority: 2, Weight: 1, State: "enabled"},
		}}
}

type upstreamLog struct {
	mu   sync.Mutex
	keys []string
}

func (l *upstreamLog) add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
}

func (l *upstreamLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

// TestFailoverBeforeFirstByteOn429 is the G2 acceptance test's gateway half:
// with two routes, a forced 429 on the first fails over to the second
// before any byte reaches the client, and the cooled-down route is skipped
// until its retry-after passes.
func TestFailoverBeforeFirstByteOn429(t *testing.T) {
	calls := &upstreamLog{}
	const stream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":4}}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Api-Key")
		calls.add(key)
		if key == "sk-provider-secret" { // route a: forced 429
			w.Header().Set("Retry-After", "30")
			w.Header().Set("Anthropic-Ratelimit-Requests-Limit", "50")
			w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "0")
			w.Header().Set("Anthropic-Ratelimit-Requests-Reset", time.Now().Add(30*time.Second).UTC().Format(time.RFC3339))
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"route a is out of requests"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()
	events := &recorder{}
	server := &Server{Upstream: map[string]string{"anthropic": upstream.URL}, Record: events.record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan: func(context.Context, Grant) (Plan, error) {
			plan := twoRoutePlan()
			plan.Routes[0].ID = "connection-1" // the leased connection is route a.
			return plan, nil
		},
		// Only pooled routes other than the leased connection read a credential.
		RouteKey: func(_ context.Context, _ Grant, r Route) ([]byte, error) {
			if r.ID != "route-b" {
				t.Errorf("credential read for %s", r.ID)
			}
			return []byte("sk-route-b-secret"), nil
		}}
	front := httptest.NewServer(server)
	defer front.Close()
	post := func() (*http.Response, string) {
		request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages",
			strings.NewReader(`{"model":"claude-opus-5-5","stream":true}`))
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		return response, string(body)
	}
	response, body := post()
	if response.StatusCode != 200 || body != stream {
		t.Fatalf("failover response: %d %q", response.StatusCode, body)
	}
	if got := calls.all(); len(got) != 2 || got[0] != "sk-provider-secret" || got[1] != "sk-route-b-secret" {
		t.Fatalf("upstream calls: %v", got)
	}
	if len(events.events) != 2 {
		t.Fatalf("events: %d", len(events.events))
	}
	first, second := events.events[0], events.events[1]
	if first.HTTPStatus != 429 || first.Status != "error" || first.RetryCount != 0 || first.RouteID != "connection-1" ||
		first.PoolID != "pool-1" || first.TTFT != nil {
		t.Fatalf("429 attempt event: %+v", first)
	}
	if second.HTTPStatus != 200 || second.Status != "ok" || second.RetryCount != 1 || second.RouteID != "route-b" ||
		second.Usage.Output != 4 {
		t.Fatalf("failover event: %+v", second)
	}
	// Route a is cooling down (retry-after 30 s, 0 requests remaining): the
	// next request goes straight to route b.
	if response, _ := post(); response.StatusCode != 200 {
		t.Fatalf("second request: %d", response.StatusCode)
	}
	if got := calls.all(); len(got) != 3 || got[2] != "sk-route-b-secret" {
		t.Fatalf("cooled route was retried: %v", got)
	}
	view := server.States.View(testGrant("anthropic", "").OrganizationID, "connection-1")
	if view.Metrics["requests"].Remaining != 0 || view.Metrics["requests"].Limit != 50 || !view.CooldownUntil.After(time.Now()) ||
		view.Last429.IsZero() || view.Breaker != BreakerClosed {
		t.Fatalf("route a state: %+v", view)
	}
}

func TestNoHeadroomPacesClientAndMidStreamFailureIsNotRetried(t *testing.T) {
	var hits int
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	events := &recorder{}
	plan := twoRoutePlan()
	plan.Routes[0].ID = "connection-1"
	server := &Server{Upstream: map[string]string{"anthropic": upstream.URL}, Record: events.record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan:     func(context.Context, Grant) (Plan, error) { return plan, nil },
		RouteKey: func(context.Context, Grant, Route) ([]byte, error) { return []byte("sk-b"), nil }}
	front := httptest.NewServer(server)
	defer front.Close()
	post := func() *http.Response {
		request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		return response
	}
	// Both routes 429: the last route's answer is the client's.
	if response := post(); response.StatusCode != http.StatusTooManyRequests || hits != 2 {
		t.Fatalf("both routes limited: %d after %d upstream calls", response.StatusCode, hits)
	}
	// Both routes are now cooling down: the gateway paces the client itself
	// with the earliest reset, without calling any provider.
	response := post()
	if response.StatusCode != http.StatusTooManyRequests || hits != 2 {
		t.Fatalf("paced: %d after %d upstream calls", response.StatusCode, hits)
	}
	if after := response.Header.Get("Retry-After"); after == "" || after == "0" {
		t.Fatalf("pacing without Retry-After: %q", after)
	}

	// A failure after the first byte is the provider's error, never retried.
	var streamed int
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		streamed++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer broken.Close()
	server2 := &Server{Upstream: map[string]string{"anthropic": broken.URL}, Record: (&recorder{}).record,
		Authorize: server.Authorize,
		Plan: func(context.Context, Grant) (Plan, error) {
			p := twoRoutePlan()
			p.Routes[0].ID = "connection-1"
			return p, nil
		},
		RouteKey: server.RouteKey}
	front2 := httptest.NewServer(server2)
	defer front2.Close()
	request, _ := http.NewRequest(http.MethodPost, front2.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","stream":true}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	if response, err := http.DefaultClient.Do(request); err == nil {
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	if streamed != 1 {
		t.Fatalf("mid-stream failure retried on another route: %d upstream calls", streamed)
	}
}

func TestCircuitBreakerOpensAndHalfOpens(t *testing.T) {
	states := NewStates()
	now := time.Now()
	states.now = func() time.Time { return now }
	r := Route{ID: "r1", Kind: KindAnthropic, State: "enabled", Priority: 1}
	plan := Plan{Routes: []Route{r}}
	for range breakerFailures {
		states.Begin("org", r, "")(http.StatusBadGateway, nil, 0)
	}
	if routes, wait := states.Order("org", plan, "m", "a", "/v1/messages", nil); len(routes) != 0 || !wait.Equal(now.Add(breakerCooldown)) {
		t.Fatalf("open breaker still selected: %v %v", routes, wait)
	}
	if v := states.View("org", "r1"); v.Breaker != BreakerOpen || v.Errors15m != breakerFailures {
		t.Fatalf("breaker view: %+v", v)
	}
	now = now.Add(breakerCooldown)
	routes, _ := states.Order("org", plan, "m", "a", "/v1/messages", nil)
	if len(routes) != 1 {
		t.Fatal("half-open breaker let no probe through")
	}
	done := states.Begin("org", r, "")
	if routes, _ := states.Order("org", plan, "m", "a", "/v1/messages", nil); len(routes) != 0 {
		t.Fatal("half-open breaker let a second probe through")
	}
	done(http.StatusOK, nil, 10)
	if v := states.View("org", "r1"); v.Breaker != BreakerClosed {
		t.Fatalf("successful probe did not close the breaker: %+v", v)
	}
	// Concurrency cap and draining: a capped route is skipped while full; a
	// draining route is used only after enabled ones.
	capped := Route{ID: "capped", Kind: KindAnthropic, State: "enabled", Priority: 1, Cap: 1}
	drain := Route{ID: "drain", Kind: KindAnthropic, State: "draining", Priority: 0}
	plan = Plan{Strategy: StrategyPriorityHeadroom, Routes: []Route{drain, capped}}
	release := states.Begin("org", capped, "")
	if routes, _ := states.Order("org", plan, "m", "a", "/v1/messages", nil); len(routes) != 1 || routes[0].ID != "drain" {
		t.Fatalf("capped route selected while full: %v", routes)
	}
	release(200, nil, 0)
	if routes, _ := states.Order("org", plan, "m", "a", "/v1/messages", nil); len(routes) != 2 || routes[0].ID != "capped" {
		t.Fatalf("draining route preferred: %v", routes)
	}
	// Bedrock needs a model mapping and serves only Messages.
	bedrock := Route{ID: "br", Kind: KindBedrock, State: "enabled", ModelMap: map[string]string{"claude-opus-5-5": "anthropic.claude-opus-5-5-v1:0"}}
	if routes, _ := states.Order("org", Plan{Routes: []Route{bedrock}}, "claude-sonnet-5", "a", "/v1/messages", nil); len(routes) != 0 {
		t.Fatal("unmapped model served on Bedrock")
	}
	if routes, _ := states.Order("org", Plan{Routes: []Route{bedrock}}, "claude-opus-5-5", "a", "/v1/models", nil); len(routes) != 0 {
		t.Fatal("Bedrock route offered an endpoint it cannot serve")
	}
}

func TestRateAndSubscriptionHeaders(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Tokens-Limit", "400000")
	h.Set("Anthropic-Ratelimit-Tokens-Remaining", "1200")
	h.Set("Anthropic-Ratelimit-Tokens-Reset", "2026-09-24T12:00:42Z")
	h.Set("X-Ratelimit-Limit-Requests", "500")
	h.Set("X-Ratelimit-Remaining-Requests", "499")
	h.Set("X-Ratelimit-Reset-Requests", "6m0s")
	h.Set("Retry-After", "7")
	metrics, wait := rateHeaders(h, now)
	if metrics["tokens"] != (Metric{400000, 1200, now.Add(42 * time.Second)}) ||
		metrics["requests"] != (Metric{500, 499, now.Add(6 * time.Minute)}) || wait != 7*time.Second {
		t.Fatalf("rate headers: %+v %v", metrics, wait)
	}
	if !metrics["tokens"].exhausted(5000, now) || metrics["tokens"].exhausted(5000, now.Add(time.Minute)) {
		t.Fatal("token exhaustion")
	}
	codex := http.Header{}
	codex.Set("X-Codex-Primary-Used-Percent", "37.5")
	codex.Set("X-Codex-Primary-Window-Minutes", "300")
	codex.Set("X-Codex-Primary-Reset-At", "1790000000")
	codex.Set("X-Codex-Secondary-Used-Percent", "12")
	codex.Set("X-Codex-Secondary-Window-Minutes", "10080")
	codex.Set("X-Codex-Secondary-Reset-After-Seconds", "3600")
	codex.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.25")
	codex.Set("Anthropic-Ratelimit-Unified-5h-Reset", "1790000100")
	windows := subscriptionWindows(codex, now)
	want := map[string]Window{
		"primary":   {"primary", 37.5, 300, time.Unix(1790000000, 0).UTC()},
		"secondary": {"secondary", 12, 10080, now.Add(time.Hour)},
		"claude_5h": {"claude_5h", 25, 300, time.Unix(1790000100, 0).UTC()},
	}
	if len(windows) != len(want) {
		t.Fatalf("windows: %+v", windows)
	}
	for _, w := range windows {
		if want[w.Name] != w {
			t.Fatalf("window %s = %+v, want %+v", w.Name, w, want[w.Name])
		}
	}
	if len(subscriptionWindows(http.Header{}, now)) != 0 {
		t.Fatal("windows invented from nothing")
	}
}

// TestClaudeSetupTokenThroughGateway: the owner's setup-token is sent as an
// OAuth bearer with the OAuth beta added to the client's own betas, never as
// x-api-key, and the sandbox only ever held the gateway token.
func TestClaudeSetupTokenThroughGateway(t *testing.T) {
	var seen http.Request
	upstream := chunkedUpstream(t, "application/json", []string{`{"model":"claude-opus-5-5","usage":{"input_tokens":1,"output_tokens":2}}`}, &seen, nil)
	defer upstream.Close()
	limits := map[string]Window{}
	server := &Server{Upstream: map[string]string{"anthropic": upstream.URL}, Record: (&recorder{}).record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			g := testGrant("anthropic", "claude-opus-5-5")
			g.Key, g.AuthMethod, g.OwnerID = []byte("sk-ant-oat01-owner-token"), access.ClaudeSetupTokenAuth, "u"
			return g, nil
		},
		RecordLimits: func(_ context.Context, g Grant, w []Window) error {
			for _, x := range w {
				limits[g.OwnerID+"/"+x.Name] = x
			}
			return nil
		},
		// A plan with pools must never be consulted for a personal route.
		Plan: func(_ context.Context, g Grant) (Plan, error) { return Plan{Routes: []Route{implicitRoute(g)}}, nil }}
	front := httptest.NewServer(server)
	defer front.Close()
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("X-Api-Key", testToken)
	request.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || seen.Header.Get("Authorization") != "Bearer sk-ant-oat01-owner-token" ||
		seen.Header.Get("X-Api-Key") != "" || seen.Header.Get("Anthropic-Beta") != "prompt-caching-2024-07-31,oauth-2025-04-20" {
		t.Fatalf("setup-token upstream headers: %d %v", response.StatusCode, seen.Header)
	}
	if implicitRoute(Grant{AuthMethod: access.ClaudeSetupTokenAuth, Provider: "anthropic"}).Kind != KindPersonal {
		t.Fatal("setup-token route is not a personal route")
	}
}

// TestCodexSubscriptionPersonalRoute: the owner's ChatGPT sign-in is served
// at the Codex backend with its account id, and its reported usage windows
// are recorded for the owner.
func TestCodexSubscriptionPersonalRoute(t *testing.T) {
	var path, auth, account string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, account = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id")
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Codex-Primary-Used-Percent", "64")
		w.Header().Set("X-Codex-Primary-Window-Minutes", "300")
		w.Header().Set("X-Codex-Primary-Reset-After-Seconds", "900")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-6-luna\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n")
	}))
	defer upstream.Close()
	var recorded []Window
	server := &Server{Upstream: map[string]string{"chatgpt": upstream.URL}, Record: (&recorder{}).record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			g := testGrant("openai", "gpt-6-luna")
			g.Key, g.AuthMethod, g.AccountID, g.OwnerID = []byte("chatgpt-access-token"), access.CodexSubscriptionAuth, "acct-1", "u"
			return g, nil
		},
		RecordLimits: func(_ context.Context, g Grant, w []Window) error {
			if g.OwnerID != "u" {
				t.Errorf("limits recorded for %q", g.OwnerID)
			}
			recorded = append(recorded, w...)
			return nil
		}}
	front := httptest.NewServer(server)
	defer front.Close()
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/openai/v1/responses", strings.NewReader(`{"model":"gpt-6-luna","stream":true}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || path != "/responses" || auth != "Bearer chatgpt-access-token" || account != "acct-1" {
		t.Fatalf("codex upstream: %d %s %q %q", response.StatusCode, path, auth, account)
	}
	if len(recorded) != 1 || recorded[0].Name != "primary" || recorded[0].UsedPct != 64 || recorded[0].WindowMinutes != 300 {
		t.Fatalf("codex windows: %+v", recorded)
	}
	// Chat Completions is not a Codex backend endpoint.
	request, _ = http.NewRequest(http.MethodPost, front.URL+"/openai/v1/chat/completions", strings.NewReader(`{"model":"gpt-6-luna"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, _ = http.DefaultClient.Do(request)
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unsupported endpoint on personal route: %d", response.StatusCode)
	}
}

func TestSigV4MatchesBotocore(t *testing.T) {
	// Expected values were produced by botocore 1.42 SigV4Auth for the same
	// requests, credentials and timestamp (AWS's documented example key).
	c := AWSCredential{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	at := time.Date(2026, 9, 25, 2, 28, 53, 0, time.UTC)
	get, _ := http.NewRequest(http.MethodGet, "https://example.amazonaws.com/", nil)
	signSigV4(get, nil, c, "us-east-1", "service", at)
	if want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260925/us-east-1/service/aws4_request, SignedHeaders=host;x-amz-date, Signature=0543aaf63e2b3c57177893f2f1afb791d7bc19afb6961d5dc7fd696682c106bb"; get.Header.Get("Authorization") != want {
		t.Fatalf("GET signature:\n got %s\nwant %s", get.Header.Get("Authorization"), want)
	}
	body := []byte(`{"max_tokens":8}`)
	c.SessionToken = "SESSIONTOKEN"
	post, _ := http.NewRequest(http.MethodPost, "https://bedrock-runtime.us-east-1.amazonaws.com/model/"+awsEscape("anthropic.claude-v2:0")+"/invoke", bytes.NewReader(body))
	post.Header.Set("Content-Type", "application/json")
	signSigV4(post, body, c, "us-east-1", "bedrock", at)
	if want := "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260925/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date;x-amz-security-token, Signature=2405a0c0070a58376fc472fc74172a0f2fb4a0d0bc199475df7064258aae475f"; post.Header.Get("Authorization") != want {
		t.Fatalf("Bedrock signature:\n got %s\nwant %s", post.Header.Get("Authorization"), want)
	}
	if post.URL.EscapedPath() != "/model/anthropic.claude-v2%3A0/invoke" || post.Header.Get("X-Amz-Security-Token") != "SESSIONTOKEN" {
		t.Fatalf("Bedrock request: %s %v", post.URL.EscapedPath(), post.Header)
	}
}

// eventFrame encodes one AWS event-stream message (test helper).
func eventFrame(headers map[string]string, payload []byte) []byte {
	var h bytes.Buffer
	for _, name := range []string{":event-type", ":content-type", ":message-type", ":exception-type"} {
		value, ok := headers[name]
		if !ok {
			continue
		}
		h.WriteByte(byte(len(name)))
		h.WriteString(name)
		h.WriteByte(7)
		_ = binary.Write(&h, binary.BigEndian, uint16(len(value)))
		h.WriteString(value)
	}
	total := 12 + h.Len() + len(payload) + 4
	frame := make([]byte, 0, total)
	frame = binary.BigEndian.AppendUint32(frame, uint32(total))
	frame = binary.BigEndian.AppendUint32(frame, uint32(h.Len()))
	frame = binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame[:8]))
	frame = append(frame, h.Bytes()...)
	frame = append(frame, payload...)
	return binary.BigEndian.AppendUint32(frame, crc32.ChecksumIEEE(frame))
}

func chunkFrame(event string) []byte {
	payload, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(event))})
	return eventFrame(map[string]string{":event-type": "chunk", ":content-type": "application/json", ":message-type": "event"}, payload)
}

// TestBedrockRoute: a pooled Bedrock route gets the Messages body without
// model or stream, with bedrock anthropic_version and anthropic_beta, a
// SigV4 Authorization from the stored AWS key, the mapped model id in the
// URL; its event stream reaches the client as Anthropic SSE and is metered.
func TestBedrockRoute(t *testing.T) {
	var seen *http.Request
	var seenBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Clone(context.Background())
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seenBody)
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		w.Header().Set("X-Amzn-Requestid", "aws-req-1")
		stream := append(chunkFrame(`{"type":"message_start","message":{"model":"claude-opus-5-5","usage":{"input_tokens":11,"output_tokens":1}}}`),
			chunkFrame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":6}}`)...)
		stream = append(stream, chunkFrame(`{"type":"message_stop"}`)...)
		for _, part := range [][]byte{stream[:5], stream[5:40], stream[40:]} { // frames split across writes
			_, _ = w.Write(part)
			w.(http.Flusher).Flush()
		}
	}))
	defer upstream.Close()
	credential, _ := json.Marshal(AWSCredential{AccessKeyID: "AKIAEXAMPLEEXAMPLE01", SecretAccessKey: "not-a-real-secret-key-for-tests-only"})
	events := &recorder{}
	route := Route{ID: "route-bedrock", Kind: KindBedrock, ConnectionID: "aws-1", AuthMethod: AWSSigV4AuthMethod, Region: "us-east-1",
		ModelMap: map[string]string{"claude-opus-5-5": "us.anthropic.claude-opus-5-5-v1:0"}, State: "enabled"}
	server := &Server{Upstream: map[string]string{KindBedrock: upstream.URL}, Record: events.record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan:     func(context.Context, Grant) (Plan, error) { return Plan{PoolID: "p", Routes: []Route{route}}, nil },
		RouteKey: func(context.Context, Grant, Route) ([]byte, error) { return append([]byte(nil), credential...), nil }}
	front := httptest.NewServer(server)
	defer front.Close()
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages",
		strings.NewReader(`{"model":"claude-opus-5-5","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	request.Header.Set("Anthropic-Beta", "fine-grained-tool-streaming-2025-05-14")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	wantSSE := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":11,\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	if response.StatusCode != 200 || string(body) != wantSSE || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("bedrock stream: %d %q %v", response.StatusCode, body, response.Header)
	}
	if seen.URL.EscapedPath() != "/model/us.anthropic.claude-opus-5-5-v1%3A0/invoke-with-response-stream" ||
		!strings.HasPrefix(seen.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AKIAEXAMPLEEXAMPLE01/") ||
		!strings.Contains(seen.Header.Get("Authorization"), "/us-east-1/bedrock/aws4_request, SignedHeaders=content-type;host;x-amz-date, Signature=") ||
		seen.Header.Get("X-Amz-Date") == "" || seen.Header.Get("X-Api-Key") != "" || seen.Header.Get("Accept") != "application/vnd.amazon.eventstream" {
		t.Fatalf("bedrock request: %s %v", seen.URL.EscapedPath(), seen.Header)
	}
	if _, ok := seenBody["model"]; ok || seenBody["stream"] != nil || seenBody["anthropic_version"] != "bedrock-2023-05-31" ||
		seenBody["max_tokens"] != float64(64) || len(seenBody["anthropic_beta"].([]any)) != 1 {
		t.Fatalf("bedrock body: %v", seenBody)
	}
	event := events.last(t)
	if event.Usage.Input != 11 || event.Usage.Output != 6 || !event.Streamed || event.RouteKind != KindBedrock || event.RouteID != "route-bedrock" {
		t.Fatalf("bedrock metering: %+v", event)
	}
	// A corrupt frame ends the stream with an SSE error, not garbage.
	bad := chunkFrame(`{"type":"ping"}`)
	bad[len(bad)-1] ^= 0xff
	if out := (&eventStreamSSE{}).Write(bad); !strings.Contains(string(out), "event: error") {
		t.Fatalf("corrupt frame: %q", out)
	}
	exception := eventFrame(map[string]string{":message-type": "exception", ":exception-type": "throttlingException"}, []byte(`{"message":"slow down"}`))
	if out := string((&eventStreamSSE{}).Write(exception)); !strings.Contains(out, "rate_limit_error") || !strings.Contains(out, "slow down") {
		t.Fatalf("exception frame: %q", out)
	}
}

// TestVertexRoute: a service-account JWT is exchanged for an access token
// (verified here against the key's public half) which is cached; the request
// goes to the model's streamRawPredict with vertex anthropic_version.
func TestVertexRoute(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	account, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "gw@example-project.iam.gserviceaccount.com",
		"private_key_id": "kid-1", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))})
	var exchanges int
	var tokenURL string
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		_ = r.ParseForm()
		parts := strings.Split(r.PostForm.Get("assertion"), ".")
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" || len(parts) != 3 {
			http.Error(w, "bad grant", 400)
			return
		}
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		signature, _ := base64.RawURLEncoding.DecodeString(parts[2])
		var claims map[string]any
		raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(raw, &claims)
		if rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature) != nil ||
			claims["iss"] != "gw@example-project.iam.gserviceaccount.com" || claims["aud"] != tokenURL ||
			claims["scope"] != "https://www.googleapis.com/auth/cloud-platform" {
			http.Error(w, "bad assertion", 401)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "ya29.test-access", "expires_in": 3599, "token_type": "Bearer"})
	}))
	defer tokens.Close()
	tokenURL = tokens.URL + "/token"
	var seenPath, seenAuth string
	var seenBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath, seenAuth = r.URL.Path, r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &seenBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","model":"claude-opus-5-5","usage":{"input_tokens":4,"output_tokens":2}}`)
	}))
	defer upstream.Close()
	route := Route{ID: "route-vertex", Kind: KindVertex, ConnectionID: "gcp-1", AuthMethod: GCPServiceAccountAuthMethod,
		Region: "us-east5", CloudProject: "example-project", ModelMap: map[string]string{"claude-opus-5-5": "claude-opus-5-5@20260901"}, State: "enabled"}
	events := &recorder{}
	server := &Server{Upstream: map[string]string{KindVertex: upstream.URL}, VertexTokenURL: tokenURL, Record: events.record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan:     func(context.Context, Grant) (Plan, error) { return Plan{Routes: []Route{route}}, nil },
		RouteKey: func(context.Context, Grant, Route) ([]byte, error) { return append([]byte(nil), account...), nil }}
	front := httptest.NewServer(server)
	defer front.Close()
	for range 2 {
		request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":8}`))
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("vertex: %d", response.StatusCode)
		}
	}
	wantPath := "/v1/projects/example-project/locations/us-east5/publishers/anthropic/models/" + url.PathEscape("claude-opus-5-5@20260901") + ":rawPredict"
	if seenPath != wantPath || seenAuth != "Bearer ya29.test-access" || exchanges != 1 {
		t.Fatalf("vertex request: %s %q exchanges=%d", seenPath, seenAuth, exchanges)
	}
	if _, ok := seenBody["model"]; ok || seenBody["anthropic_version"] != "vertex-2023-10-16" {
		t.Fatalf("vertex body: %v", seenBody)
	}
	if e := events.last(t); e.Usage.Input != 4 || e.RouteKind != KindVertex {
		t.Fatalf("vertex metering: %+v", e)
	}
	if _, err := ParseServiceAccount([]byte(`{"type":"authorized_user"}`)); err == nil {
		t.Fatal("non-service-account credential accepted")
	}
}

func TestCloudErrorsAnswerInAnthropicShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"message":"ValidationException: max_tokens too large"}`)
	}))
	defer upstream.Close()
	credential, _ := json.Marshal(AWSCredential{AccessKeyID: "AKIAEXAMPLEEXAMPLE01", SecretAccessKey: "not-a-real-secret-key-for-tests-only"})
	route := Route{ID: "br", Kind: KindBedrock, AuthMethod: AWSSigV4AuthMethod, Region: "eu-west-1",
		ModelMap: map[string]string{"claude-opus-5-5": "anthropic.claude-opus-5-5-v1:0"}, State: "enabled"}
	server := &Server{Upstream: map[string]string{KindBedrock: upstream.URL}, Record: (&recorder{}).record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan:     func(context.Context, Grant) (Plan, error) { return Plan{Routes: []Route{route}}, nil },
		RouteKey: func(context.Context, Grant, Route) ([]byte, error) { return append([]byte(nil), credential...), nil }}
	front := httptest.NewServer(server)
	defer front.Close()
	request, _ := http.NewRequest(http.MethodPost, front.URL+"/anthropic/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5"}`))
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 400 || !strings.Contains(string(body), `"type":"invalid_request_error"`) ||
		!strings.Contains(string(body), "max_tokens too large") {
		t.Fatalf("cloud error: %d %s", response.StatusCode, body)
	}
}
