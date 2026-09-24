package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "bxgw_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

func testGrant(provider, model string) Grant {
	return Grant{TokenID: "token", OrganizationID: "00000000-0000-0000-0000-000000000001", ProjectID: "p", RunID: "r",
		AttemptID: "a", TaskID: "t", StageKey: "implement", PrincipalID: "u", Harness: "claude-code",
		Provider: provider, Model: model, ConnectionID: "connection-1", Key: []byte("sk-provider-secret")}
}

type recorder struct {
	mu     sync.Mutex
	events []Event
}

func (r *recorder) record(_ context.Context, e Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *recorder) last(t *testing.T) Event {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		t.Fatal("no usage event recorded")
	}
	return r.events[len(r.events)-1]
}

// chunkedUpstream writes chunks with flushes, splitting SSE events and lines
// across writes, and holds after the first chunk until the client has read
// it, proving the gateway streams instead of buffering.
func chunkedUpstream(t *testing.T, contentType string, chunks []string, seen *http.Request, firstRead <-chan struct{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = *r.Clone(context.Background())
		body, _ := io.ReadAll(r.Body)
		seen.Body = io.NopCloser(bytes.NewReader(body))
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Request-Id", "req_upstream_1")
		w.Header().Set("Anthropic-Organization-Id", "org-secret-identity")
		w.Header().Set("Openai-Organization", "org-openai-identity")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "49")
		w.WriteHeader(http.StatusOK)
		for i, chunk := range chunks {
			_, _ = io.WriteString(w, chunk)
			w.(http.Flusher).Flush()
			if i == 0 && firstRead != nil {
				select {
				case <-firstRead:
				case <-time.After(5 * time.Second):
					t.Error("client never received the first streamed chunk before the stream ended")
				}
			}
		}
	}))
}

func split(s string, sizes ...int) []string {
	var out []string
	for _, n := range sizes {
		if n >= len(s) {
			break
		}
		out, s = append(out, s[:n]), s[n:]
	}
	return append(out, s)
}

func TestStreamingPassThroughPerAPIShape(t *testing.T) {
	cases := []struct {
		name, family, path, provider, model, body string
		stream                                    string
		want                                      Usage
		api                                       string
	}{
		{
			name: "anthropic messages with tool use and cache", family: "anthropic", path: "/anthropic/v1/messages",
			provider: "anthropic", model: "claude-opus-5-5", api: APIAnthropicMessages,
			body: `{"model":"claude-opus-5-5","stream":true,"max_tokens":64000,"tools":[{"name":"bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`,
			stream: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-opus-5-5\",\"usage\":{\"input_tokens\":12,\"cache_creation_input_tokens\":2048,\"cache_read_input_tokens\":4096,\"output_tokens\":1}}}\n\n" +
				"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"bash\",\"input\":{}}}\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"command\\\": \\\"ls\"}}\n\n" +
				": keep-alive\n\n" +
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\" -la\\\"}\"}}\n\n" +
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":57}}\n\n" +
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			want: Usage{Input: 12, Output: 57, CacheRead: 4096, CacheWrite: 2048, ServedModel: "claude-opus-5-5", Reported: true},
		},
		{
			name: "openai responses with function call", family: "openai", path: "/openai/v1/responses",
			provider: "openai", model: "gpt-6-luna", api: APIOpenAIResponses,
			body: `{"model":"gpt-6-luna","stream":true,"input":"hi","tools":[{"type":"function","name":"shell"}]}`,
			stream: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-6-luna\",\"usage\":null}}\n\n" +
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"shell\",\"call_id\":\"call_1\"}}\n\n" +
				"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"cmd\\\":\"}\n\n" +
				"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"\\\"ls\\\"}\"}\n\n" +
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-6-luna-2026-09-01\",\"usage\":{\"input_tokens\":1500,\"input_tokens_details\":{\"cached_tokens\":1000},\"output_tokens\":300,\"output_tokens_details\":{\"reasoning_tokens\":200}}}}\n\n",
			want: Usage{Input: 500, Output: 300, CacheRead: 1000, Reasoning: 200, ServedModel: "gpt-6-luna-2026-09-01", Reported: true},
		},
		{
			name: "openai chat completions with tool calls", family: "openai", path: "/openai/v1/chat/completions",
			provider: "openai", model: "gpt-6-luna", api: APIOpenAIChat,
			body: `{"model":"gpt-6-luna","stream":true,"stream_options":{"include_usage":true},"messages":[]}`,
			stream: "data: {\"id\":\"c1\",\"model\":\"gpt-6-luna\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"shell\",\"arguments\":\"\"}}]}}]}\n\n" +
				"data: {\"id\":\"c1\",\"model\":\"gpt-6-luna\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"cmd\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
				"data: {\"id\":\"c1\",\"model\":\"gpt-6-luna\",\"choices\":[],\"usage\":{\"prompt_tokens\":900,\"completion_tokens\":40,\"prompt_tokens_details\":{\"cached_tokens\":800},\"completion_tokens_details\":{\"reasoning_tokens\":10}}}\n\n" +
				"data: [DONE]\n\n",
			want: Usage{Input: 100, Output: 40, CacheRead: 800, Reasoning: 10, ServedModel: "gpt-6-luna", Reported: true},
		},
		{
			name: "opencode zen openai-compatible", family: "opencode", path: "/opencode/v1/chat/completions",
			provider: "opencode", model: "kimi-k3", api: APIOpenAIChat,
			body: `{"model":"kimi-k3","stream":true,"messages":[]}`,
			stream: "data: {\"model\":\"kimi-k3\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\r\n\r\n" +
				"data: {\"model\":\"kimi-k3\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":5,\"cache_write_tokens\":7}}\r\n\r\n" +
				"data: [DONE]\r\n\r\n",
			want: Usage{Input: 50, Output: 5, CacheWrite: 7, ServedModel: "kimi-k3", Reported: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var seen http.Request
			firstRead := make(chan struct{})
			// Odd chunk sizes cut through "data:" prefixes, JSON and CRLF pairs.
			upstream := chunkedUpstream(t, "text/event-stream; charset=utf-8", split(tc.stream, 7, 61, 3, 190, 1, 400), &seen, firstRead)
			defer upstream.Close()
			events := &recorder{}
			server := &Server{Upstream: map[string]string{tc.family: upstream.URL}, Record: events.record,
				Authorize: func(_ context.Context, token, family string) (Grant, error) {
					if token != testToken || family != tc.family {
						return Grant{}, ErrDenied
					}
					return testGrant(tc.provider, tc.model), nil
				}}
			front := httptest.NewServer(server)
			defer front.Close()
			request, _ := http.NewRequest(http.MethodPost, front.URL+tc.path, strings.NewReader(tc.body))
			request.Header.Set("Authorization", "Bearer "+testToken)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Anthropic-Beta", "prompt-caching-2024-07-31,fine-grained-tool-streaming")
			request.Header.Set("Anthropic-Version", "2023-06-01")
			request.Header.Set("User-Agent", "claude-cli/2.1.282 (external, cli)")
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			first := make([]byte, 1)
			if _, err := io.ReadFull(response.Body, first); err != nil {
				t.Fatal(err)
			}
			close(firstRead)
			rest, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(first) + string(rest); got != tc.stream {
				t.Fatalf("stream altered:\n got %q\nwant %q", got, tc.stream)
			}
			if response.Header.Get("Content-Type") != "text/event-stream; charset=utf-8" ||
				response.Header.Get("Request-Id") != "req_upstream_1" ||
				response.Header.Get("Anthropic-Ratelimit-Requests-Remaining") != "49" ||
				response.Header.Get("Anthropic-Organization-Id") != "" || response.Header.Get("Openai-Organization") != "" {
				t.Fatalf("response headers: %v", response.Header)
			}
			body, _ := io.ReadAll(seen.Body)
			if string(body) != tc.body || seen.Header.Get("Anthropic-Beta") != "prompt-caching-2024-07-31,fine-grained-tool-streaming" ||
				seen.Header.Get("User-Agent") != "claude-cli/2.1.282 (external, cli)" {
				t.Fatalf("request not passed through: %q %v", body, seen.Header)
			}
			event := events.last(t)
			if event.Usage != tc.want || event.API != tc.api || !event.Streamed || event.Status != "ok" ||
				event.HTTPStatus != 200 || event.TTFT == nil || event.RequestedModel != tc.model || event.RouteID != "connection-1" {
				t.Fatalf("metered %+v, want usage %+v", event, tc.want)
			}
		})
	}
}

func TestHeaderStrippingAndCredentialInjection(t *testing.T) {
	for _, tc := range []struct{ family, path, header, value string }{
		{"anthropic", "/anthropic/v1/messages", "X-Api-Key", "sk-provider-secret"},
		{"openai", "/openai/v1/responses", "Authorization", "Bearer sk-provider-secret"},
		{"opencode", "/opencode/v1/chat/completions", "Authorization", "Bearer sk-provider-secret"},
	} {
		t.Run(tc.family, func(t *testing.T) {
			var seen http.Request
			upstream := chunkedUpstream(t, "application/json", []string{`{"usage":{}}`}, &seen, nil)
			defer upstream.Close()
			server := &Server{Upstream: map[string]string{tc.family: upstream.URL}, Record: (&recorder{}).record,
				Authorize: func(context.Context, string, string) (Grant, error) { return testGrant(tc.family, "m"), nil }}
			front := httptest.NewServer(server)
			defer front.Close()
			request, _ := http.NewRequest(http.MethodPost, front.URL+tc.path, strings.NewReader(`{"model":"m"}`))
			for name, value := range map[string]string{
				"X-Api-Key": testToken, "Cookie": "session=1", "X-Amz-Security-Token": "aws", "X-Goog-Api-Key": "g",
				"Api-Key": "azure", "Openai-Organization": "org-choose", "Openai-Project": "proj", "X-Forwarded-For": "10.0.0.9",
				"Forwarded": "for=10.0.0.9", "Proxy-Authorization": "Basic x", "Anthropic-Version": "2023-06-01", "Openai-Beta": "responses=v1",
			} {
				request.Header.Set(name, value)
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			for _, name := range []string{"Cookie", "X-Amz-Security-Token", "X-Goog-Api-Key", "Api-Key", "Openai-Organization",
				"Openai-Project", "X-Forwarded-For", "Forwarded", "Proxy-Authorization"} {
				if seen.Header.Get(name) != "" {
					t.Errorf("%s reached the provider: %q", name, seen.Header.Get(name))
				}
			}
			for name, values := range seen.Header {
				for _, v := range values {
					if strings.Contains(v, testToken) {
						t.Errorf("gateway token forwarded upstream in %s", name)
					}
				}
			}
			if seen.Header.Get(tc.header) != tc.value || seen.Header.Get("Anthropic-Version") != "2023-06-01" ||
				seen.Header.Get("Openai-Beta") != "responses=v1" {
				t.Fatalf("credential or protocol headers: %v", seen.Header)
			}
			if tc.family == "anthropic" && seen.Header.Get("Authorization") != "" {
				t.Fatalf("anthropic got an Authorization header: %v", seen.Header)
			}
		})
	}
}

func TestNonStreamedMeteringAndErrors(t *testing.T) {
	events := &recorder{}
	var seen http.Request
	upstream := chunkedUpstream(t, "application/json",
		split(`{"id":"msg_1","type":"message","model":"claude-sonnet-5","content":[{"type":"text","text":"secret answer"}],"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":90,"output_tokens":20}}`, 30, 50),
		&seen, nil)
	defer upstream.Close()
	disabled := false
	server := &Server{Upstream: map[string]string{"anthropic": upstream.URL}, Record: events.record, TokenRate: 1000, TokenBurst: 1000,
		Authorize: func(context.Context, string, string) (Grant, error) {
			if disabled {
				return Grant{}, ErrDisabled
			}
			return testGrant("anthropic", "claude-sonnet-5"), nil
		}}
	front := httptest.NewServer(server)
	defer front.Close()
	post := func(path, body string) *http.Response {
		request, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	response := post("/anthropic/v1/messages", `{"model":"claude-sonnet-5","max_tokens":10}`)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 200 || !strings.Contains(string(body), "secret answer") {
		t.Fatalf("non-streamed response: %d %s", response.StatusCode, body)
	}
	event := events.last(t)
	want := Usage{Input: 10, Output: 20, CacheRead: 90, ServedModel: "claude-sonnet-5", Reported: true}
	if event.Usage != want || event.Streamed || event.Price.Version == "" || Cost(event.Usage, event.Price.Rates) == 0 {
		t.Fatalf("non-streamed metering: %+v", event)
	}
	// A model other than the lease's is refused before any upstream call.
	count := len(events.events)
	if response := post("/anthropic/v1/messages", `{"model":"claude-fable-5-1"}`); response.StatusCode != http.StatusForbidden {
		t.Fatalf("unapproved model: %d", response.StatusCode)
	}
	// Endpoints beyond model calls are not reachable with a token.
	if response := post("/anthropic/v1/organizations/me", `{}`); response.StatusCode != http.StatusNotFound {
		t.Fatalf("non-model endpoint: %d", response.StatusCode)
	}
	if response := post("/nope/v1/messages", `{}`); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown family: %d", response.StatusCode)
	}
	// Master switch off: a clear provider-shaped error, not a silent failure.
	disabled = true
	response = post("/anthropic/v1/messages", `{"model":"claude-sonnet-5"}`)
	body, _ = io.ReadAll(response.Body)
	if response.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "disabled by your admin") ||
		!strings.Contains(string(body), `"type":"error"`) {
		t.Fatalf("disabled gateway: %d %s", response.StatusCode, body)
	}
	if len(events.events) != count {
		t.Fatalf("refused requests reached metering: %d events", len(events.events))
	}
}

func TestRateLimitsPerTokenAndOrg(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "{}") }))
	defer upstream.Close()
	server := &Server{Upstream: map[string]string{"openai": upstream.URL}, Record: (&recorder{}).record,
		TokenRate: 0.001, TokenBurst: 2, OrgRate: 1000, OrgBurst: 1000,
		Authorize: func(context.Context, string, string) (Grant, error) { return testGrant("openai", "m"), nil }}
	front := httptest.NewServer(server)
	defer front.Close()
	codes := []int{}
	for range 3 {
		request, _ := http.NewRequest(http.MethodPost, front.URL+"/openai/v1/responses", strings.NewReader(`{"model":"m"}`))
		request.Header.Set("Authorization", "Bearer "+testToken)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode == http.StatusTooManyRequests && response.Header.Get("Retry-After") == "" {
			t.Fatal("429 without Retry-After")
		}
		response.Body.Close()
		codes = append(codes, response.StatusCode)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("per-token limit: %v", codes)
	}
	org := newLimiter(1, 1)
	now := time.Now()
	if ok, _ := org.allow("org", now); !ok {
		t.Fatal("first org request limited")
	}
	if ok, wait := org.allow("org", now); ok || wait <= 0 {
		t.Fatal("org burst not enforced")
	}
	if ok, _ := org.allow("org", now.Add(time.Second)); !ok {
		t.Fatal("org bucket did not refill")
	}
}

func TestCostMath(t *testing.T) {
	// Claude Opus 5.5 list: $4 in, $20 out, $0.20 cache read, $5 cache write per MTok.
	price := ManifestPrice("anthropic", "claude-opus-5-5")
	if price.Version != "manifest:2026-09-24" || price.Rates != (Rates{4_000_000, 20_000_000, 200_000, 5_000_000}) {
		t.Fatalf("manifest price: %+v", price)
	}
	u := Usage{Input: 1_000_000, Output: 500_000, CacheRead: 2_000_000, CacheWrite: 100_000}
	// 4 + 10 + 0.40 + 0.50 = $14.90
	if got := Cost(u, price.Rates); got != 14_900_000 {
		t.Fatalf("cost = %d micros, want 14900000", got)
	}
	if got := Cost(Usage{Input: 1}, Rates{Input: 400_000}); got != 0 { // $0.0000004 rounds to 0 micros
		t.Fatalf("rounding: %d", got)
	}
	if got := Cost(Usage{Input: 3}, Rates{Input: 500_000}); got != 2 { // 1.5 micros rounds half up
		t.Fatalf("half-up rounding: %d", got)
	}
	if ManifestPrice("openai", "unknown-model").Version != "" {
		t.Fatal("unknown models must be unpriced, not guessed")
	}
	if dated := ManifestPrice("anthropic", "claude-haiku-4-5-20251001"); dated.Rates.Input != 1_000_000 {
		t.Fatalf("dated snapshot price: %+v", dated)
	}
}

func TestMeterIgnoresOversizedAndMalformedEvents(t *testing.T) {
	m := newMeter(APIAnthropicMessages, true)
	m.Write([]byte("data: {not json}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n"))
	m.Write([]byte("data: " + strings.Repeat("x", maxEventBytes+1) + "\n\n"))
	m.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}")) // no trailing blank line
	if got := m.Finish(); got.Output != 9 || !got.Reported {
		t.Fatalf("meter: %+v", got)
	}
	if got := newMeter(APIOpenAIChat, true).Finish(); got.Reported {
		t.Fatal("absent usage reported")
	}
}
