package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Claude Code through the gateway: a 429 with retry-after is retried by the
// CLI itself, a streamed tool_use round-trips (the tool runs locally and its
// result comes back in the next request), and cache usage is metered.
func TestHarnessProbeClaudeCodeToolUseAnd429(t *testing.T) {
	binary := requireProbe(t, "claude")
	const model = "claude-sonnet-5"
	probeDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(probeDir, "probe.txt"), []byte("probe-file-contents\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	quoted, _ := json.Marshal(filepath.Join(probeDir, "probe.txt"))
	probeFile := string(quoted)
	var mu sync.Mutex
	var calls []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		n := len(calls)
		hasResult := strings.Contains(string(body), `"tool_result"`) && strings.Contains(string(body), "probe-file-contents")
		calls = append(calls, fmt.Sprintf("%s %s tool_result=%t", r.Method, r.URL.Path, hasResult))
		mu.Unlock()
		switch {
		case n == 0:
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
		case !hasResult:
			sse(w,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_t\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+model+"\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":30,\"cache_creation_input_tokens\":1000,\"cache_read_input_tokens\":0,\"output_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_probe\",\"name\":\"Read\",\"input\":{}}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file_path\\\": \"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\""+strings.ReplaceAll(probeFile, `"`, `\"`)+"}\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":20}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			sse(w,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_u\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+model+"\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":40,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":1000,\"output_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"tool-probe-done\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":6}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		}
	}))
	defer upstream.Close()
	events := &recorder{}
	server := &Server{Upstream: map[string]string{"anthropic": upstream.URL}, Record: events.record,
		Authorize: func(_ context.Context, token, family string) (Grant, error) {
			g := testGrant("anthropic", model)
			g.Key = []byte("sk-mock-provider")
			return g, nil
		}}
	front := httptest.NewServer(server)
	defer front.Close()
	home := t.TempDir()
	token := probeToken(t)
	env := append(baseEnv(home), "ANTHROPIC_BASE_URL="+front.URL+"/anthropic", "ANTHROPIC_AUTH_TOKEN="+token, "ANTHROPIC_API_KEY="+token)
	out := runProbe(t, binary, probeDir, env, "--bare", "--settings", `{"availableModels":["`+model+`"],"fallbackModel":[]}`,
		"--model", model, "--print", "--output-format", "stream-json", "--verbose", "--", "Read probe.txt")
	mu.Lock()
	t.Logf("upstream calls: %v", calls)
	got := append([]string(nil), calls...)
	mu.Unlock()
	if len(got) < 3 || !strings.Contains(got[len(got)-1], "tool_result=true") || !strings.Contains(out, "tool-probe-done") {
		t.Fatalf("429 retry and tool round trip did not complete: %v", got)
	}
	var cacheWrite, cacheRead int64
	var saw429 bool
	for _, e := range events.events {
		cacheWrite += e.Usage.CacheWrite
		cacheRead += e.Usage.CacheRead
		saw429 = saw429 || e.HTTPStatus == 429
	}
	if !saw429 || cacheWrite != 1000 || cacheRead != 1000 {
		t.Fatalf("metering: 429=%t cacheWrite=%d cacheRead=%d", saw429, cacheWrite, cacheRead)
	}
}
