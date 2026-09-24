package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
)

// Families are the URL prefixes a harness is pointed at: <base>/<family>/v1/…
// maps to the provider's own /v1/… on its approved origin.
var Families = []string{"anthropic", "openai", "opencode", "opencode-go"}

// RouteKind is the §4 route kind for a G1 one-connection route.
func RouteKind(family string) string {
	switch family {
	case "opencode":
		return "opencode_zen"
	case "opencode-go":
		return "opencode_go"
	}
	return family
}

// upstreamBase is the provider origin without its /v1 suffix.
func upstreamBase(family string) string {
	return strings.TrimSuffix(access.ModelOrigin(family), "/v1")
}

// maxRequestBytes bounds a request body: long agent transcripts with images
// fit, anything larger is refused before it reaches a provider.
const maxRequestBytes = 32 << 20

// Server is the gateway's HTTP handler.
type Server struct {
	DB        *pgxpool.Pool
	Authorize func(ctx context.Context, token, family string) (Grant, error)
	Prices    *PriceBook
	// Upstream overrides a family's provider base URL (tests and mock
	// probes only); production uses the approved origins.
	Upstream map[string]string
	Client   *http.Client
	// Per-token and per-organization request rates (requests/second).
	TokenRate, OrgRate   float64
	TokenBurst, OrgBurst int
	// Record overrides persisting events (tests).
	Record func(context.Context, Event) error

	tokens, orgs *limiter
	inflight     atomic.Int64
	started      atomic.Bool
}

// InFlight is the number of requests currently being proxied, used to drain.
func (s *Server) InFlight() int64 { return s.inflight.Load() }

func (s *Server) init() {
	if s.started.CompareAndSwap(false, true) {
		s.tokens = newLimiter(s.TokenRate, max(s.TokenBurst, 1))
		s.orgs = newLimiter(s.OrgRate, max(s.OrgBurst, 1))
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.init()
	family, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok || !allowedFamily(family) {
		http.NotFound(w, r)
		return
	}
	path := "/" + rest
	if !allowedEndpoint(family, r.Method, path) {
		writeError(w, family, http.StatusNotFound, "not_found_error", "This endpoint is not available through the Blaxsmith model gateway.")
		return
	}
	token := bearer(r)
	now := time.Now()
	if token == "" {
		writeError(w, family, http.StatusUnauthorized, "authentication_error", "Missing Blaxsmith gateway token.")
		return
	}
	hash := HashToken(token)
	if ok, wait := s.tokens.allow(string(hash[:]), now); !ok {
		tooMany(w, family, wait)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		writeError(w, family, http.StatusRequestEntityTooLarge, "request_too_large", "Request body is too large for the gateway.")
		return
	}
	grant, err := s.Authorize(r.Context(), token, family)
	switch {
	case errors.Is(err, ErrDisabled):
		writeError(w, family, http.StatusForbidden, "permission_error", "The Blaxsmith model gateway is disabled by your admin.")
		return
	case errors.Is(err, ErrDenied):
		writeError(w, family, http.StatusUnauthorized, "authentication_error", "Gateway token is invalid, expired, or revoked.")
		return
	case err != nil:
		log.Printf("gateway authorize: %v", err)
		writeError(w, family, http.StatusServiceUnavailable, "api_error", "The gateway could not check this token.")
		return
	}
	defer clear(grant.Key)
	if ok, wait := s.orgs.allow(grant.OrganizationID, now); !ok {
		tooMany(w, family, wait)
		return
	}
	requested, streamed := requestModel(body)
	if r.Method == http.MethodPost && requested != "" && requested != grant.Model {
		writeError(w, family, http.StatusForbidden, "permission_error",
			"Model "+strconv.Quote(requested)+" is not approved for this stage; it may use "+strconv.Quote(grant.Model)+".")
		return
	}
	s.proxy(w, r, family, path, body, grant, requested, streamed, now)
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, family, path string, body []byte, grant Grant,
	requested string, streamed bool, started time.Time) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	base := upstreamBase(family)
	if override := s.Upstream[family]; override != "" {
		base = override
	}
	target, err := url.Parse(base + path)
	if err != nil || base == "" {
		writeError(w, family, http.StatusBadGateway, "api_error", "No approved upstream for this provider.")
		return
	}
	target.RawQuery = r.URL.RawQuery
	upstream, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, family, http.StatusBadGateway, "api_error", "Upstream request could not be built.")
		return
	}
	copyRequestHeaders(upstream.Header, r.Header)
	injectCredential(upstream.Header, family, path, grant.Key)
	upstream.ContentLength = int64(len(body))
	client := s.Client
	if client == nil {
		client = defaultClient
	}
	event := Event{Grant: grant, RouteID: grant.ConnectionID, RouteKind: RouteKind(family), API: apiFor(path),
		RequestedModel: requested, StartedAt: started, Streamed: streamed}
	defer func() {
		event.Duration = time.Since(started)
		event.Price = s.Prices.Lookup(context.WithoutCancel(r.Context()), grant.OrganizationID, grant.Provider,
			firstNonEmpty(event.Usage.ServedModel, requested, grant.Model), started)
		record := s.Record
		if record == nil {
			record = func(ctx context.Context, e Event) error { return Record(ctx, s.DB, e) }
		}
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
		defer cancel()
		if err := record(ctx, event); err != nil {
			log.Printf("gateway meter: %v", err) // never the token, key, or body.
		}
	}()
	response, err := client.Do(upstream)
	if err != nil {
		event.Status, event.HTTPStatus = "error", http.StatusBadGateway
		if r.Context().Err() != nil {
			event.Status = "cancelled"
		}
		writeError(w, family, http.StatusBadGateway, "api_error", "The provider could not be reached.")
		return
	}
	defer response.Body.Close()
	event.HTTPStatus = response.StatusCode
	event.RequestID = requestID(response.Header)
	contentType := response.Header.Get("Content-Type")
	event.Streamed = strings.HasPrefix(contentType, "text/event-stream")
	copyResponseHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	meter := newMeter(event.API, event.Streamed)
	if response.StatusCode >= 300 || !strings.Contains(contentType, "json") && !event.Streamed {
		meter = newMeter(APIOther, false) // error bodies carry no usage worth parsing.
	}
	buffer := make([]byte, 32<<10)
	status := "ok"
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			if event.TTFT == nil {
				ttft := time.Since(started)
				event.TTFT = &ttft
			}
			meter.Write(buffer[:n])
			if _, err := w.Write(buffer[:n]); err != nil {
				status = "cancelled"
				break
			}
			if flusher != nil {
				flusher.Flush() // keep SSE framing and keep-alives on the provider's timing.
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			status = "error"
			if r.Context().Err() != nil {
				status = "cancelled"
			}
			break
		}
	}
	event.Usage = meter.Finish()
	if response.StatusCode >= 400 && status == "ok" {
		status = "error"
	}
	event.Status = status
}

var defaultClient = &http.Client{Transport: &http.Transport{
	Proxy:                 nil,
	ForceAttemptHTTP2:     true,
	MaxIdleConnsPerHost:   64,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: time.Second,
	// No response-header timeout: a non-streamed long reasoning request can
	// legitimately take many minutes before its first byte.
}}

func allowedFamily(family string) bool {
	for _, f := range Families {
		if f == family {
			return true
		}
	}
	return false
}

// allowedEndpoint limits each family to model calls (§12: tokens grant no
// access to anything but model calls for their families).
func allowedEndpoint(family, method, path string) bool {
	if strings.Contains(path, "..") || strings.Contains(path, "//") {
		return false
	}
	if method == http.MethodGet && (path == "/v1/models" || strings.HasPrefix(path, "/v1/models/")) {
		return true
	}
	if method != http.MethodPost {
		return false
	}
	switch family {
	case "anthropic":
		return path == "/v1/messages" || path == "/v1/messages/count_tokens"
	case "openai":
		return path == "/v1/responses" || path == "/v1/responses/compact" || path == "/v1/chat/completions"
	case "opencode", "opencode-go":
		return path == "/v1/chat/completions" || path == "/v1/responses" || path == "/v1/messages"
	}
	return false
}

// bearer reads the gateway token from Authorization: Bearer (Claude Code's
// ANTHROPIC_AUTH_TOKEN, Codex, OpenAI-compatible SDKs) or x-api-key
// (Anthropic SDKs given the token as an API key).
func bearer(r *http.Request) string {
	if value, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(value)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

func requestModel(body []byte) (string, bool) {
	var request struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if len(body) == 0 || json.Unmarshal(body, &request) != nil {
		return "", false
	}
	return request.Model, request.Stream
}

func requestID(h http.Header) string {
	for _, name := range []string{"Request-Id", "X-Request-Id", "Cf-Ray"} {
		if v := h.Get(name); v != "" && len(v) <= 200 && !strings.ContainsAny(v, "\r\n") {
			return v
		}
	}
	return ""
}

func tooMany(w http.ResponseWriter, family string, wait time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(wait.Seconds(), 1)))))
	writeError(w, family, http.StatusTooManyRequests, "rate_limit_error", "Gateway request rate limit reached; retry after the indicated delay.")
}

// writeError answers in the provider's own error shape so each harness
// reports it (and retries 429s) as it would a provider error.
func writeError(w http.ResponseWriter, family string, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	var body any
	if family == "anthropic" {
		body = map[string]any{"type": "error", "error": map[string]string{"type": kind, "message": message}}
	} else {
		body = map[string]any{"error": map[string]any{"message": message, "type": kind, "code": kind}}
	}
	_ = json.NewEncoder(w).Encode(body)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
