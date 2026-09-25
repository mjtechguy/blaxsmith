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
	"time"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

// proxy sends one authorized request through its plan's routes (§4): the
// best route first, failing over to the next only before the first byte
// reaches the client, on 429 (with retry-after), 408, 5xx, a connect error
// or a timeout, up to two retries. A mid-stream failure is the provider's
// error; harnesses retry themselves.
func (s *Server) proxy(w http.ResponseWriter, r *http.Request, family, path string, body []byte, grant Grant,
	requested string, streamed bool, started time.Time) {
	s.inflight.Add(1)
	defer s.inflight.Add(-1)
	plan := Plan{Routes: []Route{implicitRoute(grant)}}
	if s.Plan != nil {
		p, err := s.Plan(r.Context(), grant)
		if err != nil {
			log.Printf("gateway route plan: %v", err)
			writeError(w, family, http.StatusServiceUnavailable, "api_error", "The gateway could not pick a route for this request.")
			return
		}
		plan = p
	}
	ids := make([]string, 0, len(plan.Routes))
	for _, route := range plan.Routes {
		ids = append(ids, route.ID)
	}
	// Every replica's cooldowns and breakers, read with a short TTL.
	views := s.Shared.Views(r.Context(), grant.OrganizationID, ids)
	routes, wait := s.States.Order(grant.OrganizationID, plan, firstNonEmpty(requested, grant.Model), grant.AttemptID, path, views)
	if len(routes) == 0 && wait.IsZero() {
		writeError(w, family, http.StatusNotFound, "not_found_error", "No route for this stage serves this model or endpoint.")
		return
	}
	if len(routes) == 0 {
		// Every route is cooling down, saturated or open: pace the client
		// with the earliest reset instead of calling a provider (§5).
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(max(time.Until(wait).Seconds(), 1)))))
		writeError(w, family, http.StatusTooManyRequests, "rate_limit_error",
			"No route has headroom for this request right now; retry after the indicated delay.")
		return
	}
	tries := 0
	for i, route := range routes {
		// Across replicas: the breaker's single probe, concurrency slots and
		// configured per-minute quotas. A route without one is skipped.
		admitted, err := s.Shared.admit(r.Context(), grant.OrganizationID, plan, route, views[route.ID])
		if err != nil {
			continue
		}
		event := Event{Grant: grant, PoolID: plan.PoolID, RouteID: route.ID, RouteKind: route.Kind, API: apiFor(path),
			RequestedModel: requested, StartedAt: time.Now(), Streamed: streamed, RetryCount: tries}
		if tries == 0 {
			event.StartedAt = started
		}
		tries++
		if s.try(w, r, family, path, body, grant, route, &event, i == len(routes)-1, admitted) {
			return
		}
	}
	// Every route was busy on other replicas, or failed over without an answer.
	w.Header().Set("Retry-After", "1")
	writeError(w, family, http.StatusTooManyRequests, "rate_limit_error",
		"No route has headroom for this request right now; retry after the indicated delay.")
}

// retryable statuses fail over to the next route before the first byte (§4).
func retryable(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500
}

// try sends the request on one route. It returns false, having written
// nothing to the client, when the request should fail over to the next
// route; otherwise it has answered the client. Every sent request is
// metered as its own usage event.
func (s *Server) try(w http.ResponseWriter, r *http.Request, family, path string, body []byte, grant Grant,
	route Route, event *Event, last bool, admitted func()) bool {
	started := event.StartedAt
	upstream, eventStream, err := s.upstreamRequest(r, family, path, body, grant, route)
	if err != nil {
		admitted()
		log.Printf("gateway route %s: %v", route.ID, err) // never the credential or body.
		if last {
			writeError(w, family, http.StatusBadGateway, "api_error", "The gateway could not build a request for this route.")
		}
		return last
	}
	release := s.States.Begin(grant.OrganizationID, route, grant.AttemptID)
	var header http.Header
	status := 0
	defer func() {
		u := event.Usage
		tokens := u.Input + u.Output + u.CacheRead + u.CacheWrite
		outcome := release(status, header, tokens)
		admitted()
		if s.Shared != nil {
			if err := s.Shared.Observe(r.Context(), grant.OrganizationID, route.ID, status, outcome.CooldownUntil, outcome.Opened); err != nil {
				log.Printf("gateway shared route state: %v", err)
			}
			s.Shared.used(r.Context(), grant.OrganizationID, route, tokens)
		}
		s.finish(r.Context(), event, started)
	}()
	client := s.Client
	if client == nil {
		client = defaultClient
	}
	response, err := client.Do(upstream)
	if err != nil {
		event.Status, event.HTTPStatus = "error", http.StatusBadGateway
		if r.Context().Err() != nil {
			event.Status = "cancelled"
			return true
		}
		if !last {
			return false
		}
		writeError(w, family, http.StatusBadGateway, "api_error", "The provider could not be reached.")
		return true
	}
	defer response.Body.Close()
	status, header = response.StatusCode, response.Header
	event.HTTPStatus = response.StatusCode
	event.RequestID = requestID(response.Header)
	if route.Personal() {
		s.recordLimits(r.Context(), grant, subscriptionWindows(response.Header, time.Now()))
	}
	if !last && retryable(response.StatusCode) {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		event.Status = "error"
		return false
	}
	if response.StatusCode >= 400 && (route.Kind == KindBedrock || route.Kind == KindVertex) {
		// Cloud error bodies are not Anthropic's shape; answer in it so the
		// harness handles (and retries) the error as it would Anthropic's.
		if wait := retryAfter(response.Header, time.Now()); wait > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		}
		writeError(w, family, response.StatusCode, errorKind(response.StatusCode), cloudErrorMessage(response.Body))
		event.Status = "error"
		return true
	}
	contentType := response.Header.Get("Content-Type")
	var convert *eventStreamSSE
	if eventStream && response.StatusCode == http.StatusOK {
		convert, contentType = &eventStreamSSE{}, "text/event-stream"
	}
	event.Streamed = strings.HasPrefix(contentType, "text/event-stream")
	copyResponseHeaders(w.Header(), response.Header)
	if convert != nil {
		w.Header().Set("Content-Type", contentType)
	}
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
	outcome := "ok"
	for {
		n, readErr := response.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			if convert != nil {
				chunk = convert.Write(chunk)
			}
			if event.TTFT == nil {
				ttft := time.Since(started)
				event.TTFT = &ttft
			}
			meter.Write(chunk)
			if _, err := w.Write(chunk); err != nil {
				outcome = "cancelled"
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
			outcome = "error"
			if r.Context().Err() != nil {
				outcome = "cancelled"
			}
			break
		}
	}
	event.Usage = meter.Finish()
	if response.StatusCode >= 400 && outcome == "ok" {
		outcome = "error"
	}
	event.Status = outcome
	return true
}

// finish prices and records one upstream request.
func (s *Server) finish(ctx context.Context, event *Event, started time.Time) {
	event.Duration = time.Since(started)
	g := event.Grant
	event.Price = s.Prices.Lookup(context.WithoutCancel(ctx), g.OrganizationID, g.Provider,
		firstNonEmpty(event.Usage.ServedModel, event.RequestedModel, g.Model), started)
	record := s.Record
	if record == nil {
		record = func(ctx context.Context, e Event) error { return Record(ctx, s.DB, e) }
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := record(recordCtx, *event); err != nil {
		log.Printf("gateway meter: %v", err) // never the token, key, or body.
	}
}

// recordLimits stores a personal route's reported windows for its owner.
func (s *Server) recordLimits(ctx context.Context, g Grant, windows []Window) {
	if len(windows) == 0 {
		return
	}
	record := s.RecordLimits
	if record == nil {
		record = func(ctx context.Context, g Grant, w []Window) error { return RecordSubscriptionLimits(ctx, s.DB, g, w) }
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := record(recordCtx, g, windows); err != nil {
		log.Printf("gateway subscription limits: %v", err)
	}
}

// chatGPTCodexBase is where Codex itself sends a ChatGPT-plan sign-in's requests.
const chatGPTCodexBase = "https://chatgpt.com/backend-api/codex"

// upstreamRequest builds the provider request for one route: its URL, body,
// and the route's own credential, the only one sent. It reports whether the
// response is an AWS event stream to re-frame as SSE.
func (s *Server) upstreamRequest(r *http.Request, family, path string, body []byte, grant Grant, route Route) (*http.Request, bool, error) {
	ctx := r.Context()
	key := grant.Key
	if route.ID != grant.ConnectionID {
		if s.RouteKey == nil {
			return nil, false, errors.New("no route credential reader")
		}
		var err error
		if key, err = s.RouteKey(ctx, grant, route); err != nil {
			return nil, false, err
		}
		defer clear(key)
	}
	ours := firstNonEmpty(requestedOrEmpty(body), grant.Model)
	model, ok := route.model(ours)
	if !ok {
		return nil, false, errors.New("route does not serve the model")
	}
	override := func(name, fallback string) string {
		if v := s.Upstream[name]; v != "" {
			return v
		}
		return fallback
	}
	switch route.Kind {
	case KindBedrock:
		credential, err := ParseAWSCredential(key)
		if err != nil {
			return nil, false, err
		}
		out, stream, err := bedrockBody(body, headerList(r.Header, "Anthropic-Beta"))
		if err != nil {
			return nil, false, err
		}
		action := "/invoke"
		if stream {
			action = "/invoke-with-response-stream"
		}
		upstream, err := http.NewRequestWithContext(ctx, http.MethodPost,
			override(KindBedrock, bedrockEndpoint(route.Region))+"/model/"+awsEscape(model)+action, bytes.NewReader(out))
		if err != nil {
			return nil, false, err
		}
		upstream.Header.Set("Content-Type", "application/json")
		upstream.Header.Set("Accept", "application/json")
		if stream {
			upstream.Header.Set("Accept", "application/vnd.amazon.eventstream")
		}
		upstream.ContentLength = int64(len(out))
		signSigV4(upstream, out, credential, route.Region, "bedrock", time.Now())
		return upstream, stream, nil
	case KindVertex:
		client := s.Client
		if client == nil {
			client = defaultClient
		}
		token, err := s.google.token(ctx, client, firstNonEmpty(s.VertexTokenURL, googleTokenURL), key)
		if err != nil {
			return nil, false, err
		}
		out, stream, err := vertexBody(body)
		if err != nil {
			return nil, false, err
		}
		upstream, err := http.NewRequestWithContext(ctx, http.MethodPost,
			override(KindVertex, vertexEndpoint(route.Region))+vertexPath(route.CloudProject, route.Region, model, stream),
			bytes.NewReader(out))
		if err != nil {
			return nil, false, err
		}
		copyRequestHeaders(upstream.Header, r.Header)
		upstream.Header.Del("Anthropic-Version") // it is in the body for Vertex.
		upstream.Header.Set("Content-Type", "application/json")
		upstream.Header.Set("Authorization", "Bearer "+token)
		upstream.ContentLength = int64(len(out))
		return upstream, false, nil
	}
	base := override(family, upstreamBase(family))
	target := path
	if route.Personal() && route.AuthMethod == access.CodexSubscriptionAuth {
		// The owner's ChatGPT-plan sign-in is served where Codex itself
		// sends it: the Codex backend, without the /v1 prefix.
		base, target = override("chatgpt", chatGPTCodexBase), strings.TrimPrefix(path, "/v1")
	}
	if base == "" {
		return nil, false, errors.New("no approved upstream for this provider")
	}
	if model != ours {
		var err error
		if body, err = withModel(body, model); err != nil {
			return nil, false, err
		}
	}
	u, err := url.Parse(base + target)
	if err != nil {
		return nil, false, err
	}
	u.RawQuery = r.URL.RawQuery
	upstream, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	copyRequestHeaders(upstream.Header, r.Header)
	injectCredential(upstream.Header, family, path, key, route.AuthMethod, grant.AccountID)
	upstream.ContentLength = int64(len(body))
	return upstream, false, nil
}

func requestedOrEmpty(body []byte) string {
	model, _ := requestModel(body)
	return model
}

// withModel replaces the body's model with the route's id for it.
func withModel(body []byte, model string) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	fields["model"], _ = json.Marshal(model)
	return json.Marshal(fields)
}

func errorKind(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status < 500:
		return "invalid_request_error"
	case status == 529 || status == http.StatusServiceUnavailable:
		return "overloaded_error"
	}
	return "api_error"
}

// cloudErrorMessage reads the message from an AWS ({"message"}) or Google
// ({"error":{"message"}}) error body, bounded.
func cloudErrorMessage(body io.Reader) string {
	raw, _ := io.ReadAll(io.LimitReader(body, 16<<10))
	var e struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &e)
	message := firstNonEmpty(e.Message, e.Error.Message, "The cloud provider returned an error.")
	if len(message) > 500 {
		message = message[:500]
	}
	return message
}
