package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Harness probes (plan §13): run each real CLI, credential-free, against a
// mock upstream through the gateway. Opt in with BLAXSMITH_HARNESS_PROBE=1;
// each probe skips when its binary is not on PATH.

type probeLog struct {
	mu       sync.Mutex
	gateway  []string // what the harness sent to the gateway
	upstream []string // what the provider received
}

func (l *probeLog) add(to *[]string, line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	*to = append(*to, line)
}

func (l *probeLog) snapshot() ([]string, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.gateway...), append([]string(nil), l.upstream...)
}

func authKind(h http.Header) string {
	switch {
	case strings.HasPrefix(h.Get("Authorization"), "Bearer "+TokenPrefix):
		return "bearer:gateway-token"
	case h.Get("X-Api-Key") != "" && strings.HasPrefix(h.Get("X-Api-Key"), TokenPrefix):
		return "x-api-key:gateway-token"
	case h.Get("X-Api-Key") == "sk-mock-provider":
		return "x-api-key:provider-key"
	case h.Get("Authorization") == "Bearer sk-mock-provider":
		return "bearer:provider-key"
	case h.Get("Authorization") != "" || h.Get("X-Api-Key") != "":
		return "other-credential"
	}
	return "none"
}

func sse(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	for _, e := range events {
		_, _ = io.WriteString(w, e)
		w.(http.Flusher).Flush()
		time.Sleep(5 * time.Millisecond)
	}
}

func mockUpstream(t *testing.T, log *probeLog) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &req)
		log.add(&log.upstream, fmt.Sprintf("%s %s model=%s stream=%t auth=%s beta=%q", r.Method, r.URL.Path, req.Model, req.Stream,
			authKind(r.Header), r.Header.Get("Anthropic-Beta")))
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/messages") && req.Stream:
			sse(w,
				"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_p\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\""+req.Model+"\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":21,\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":0,\"output_tokens\":1}}}\n\n",
				"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
				"event: ping\ndata: {\"type\":\"ping\"}\n\n",
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"probe-ok\"}}\n\n",
				"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
				"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":7}}\n\n",
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case strings.HasSuffix(r.URL.Path, "/v1/messages"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_p","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":"probe-ok"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":2}}`, req.Model)
		case strings.HasSuffix(r.URL.Path, "/v1/responses"):
			sse(w,
				"event: response.created\ndata: {\"type\":\"response.created\",\"sequence_number\":0,\"response\":{\"id\":\"resp_p\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\""+req.Model+"\",\"output\":[]}}\n\n",
				"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"sequence_number\":1,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n",
				"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"probe-ok\"}\n\n",
				"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"sequence_number\":3,\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"probe-ok\",\"annotations\":[]}]}}\n\n",
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"sequence_number\":4,\"response\":{\"id\":\"resp_p\",\"object\":\"response\",\"status\":\"completed\",\"model\":\""+req.Model+"\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"probe-ok\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":30,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":4,\"output_tokens_details\":{\"reasoning_tokens\":0},\"total_tokens\":34}}}\n\n")
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			sse(w,
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\""+req.Model+"\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"probe-ok\"},\"finish_reason\":null}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\""+req.Model+"\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
				"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\""+req.Model+"\",\"choices\":[],\"usage\":{\"prompt_tokens\":40,\"completion_tokens\":3,\"total_tokens\":43}}\n\n",
				"data: [DONE]\n\n")
		case strings.Contains(r.URL.Path, "/v1/models"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[],"object":"list"}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

// probeGateway fronts the mock with the real gateway handler. The grant
// allows model; everything the harness sends is logged before authorization.
func probeGateway(t *testing.T, family, model string, log *probeLog, events *recorder) *httptest.Server {
	upstream := mockUpstream(t, log)
	t.Cleanup(upstream.Close)
	server := &Server{Upstream: map[string]string{family: upstream.URL}, Record: events.record,
		Authorize: func(_ context.Context, token, fam string) (Grant, error) {
			if !validToken(token) || fam != family {
				return Grant{}, ErrDenied
			}
			g := testGrant(family, model)
			g.Key = []byte("sk-mock-provider")
			return g, nil
		}}
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		log.add(&log.gateway, fmt.Sprintf("%s %s model=%s auth=%s ua=%q", r.Method, r.URL.Path, req.Model, authKind(r.Header), r.UserAgent()))
		r.Body = io.NopCloser(bytes.NewReader(body))
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)
	return front
}

func probeToken(t *testing.T) string {
	token, _, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func runProbe(t *testing.T, binary string, dir string, env []string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(tenant.System(t.Context()), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env = dir, env
	out, err := cmd.CombinedOutput()
	t.Logf("%s exit=%v\n%s", filepath.Base(binary), err, strings.TrimSpace(string(out)))
	return string(out)
}

func requireProbe(t *testing.T, name string) string {
	if os.Getenv("BLAXSMITH_HARNESS_PROBE") != "1" {
		t.Skip("set BLAXSMITH_HARNESS_PROBE=1 to run real harness probes")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed", name)
	}
	return path
}

func baseEnv(home string) []string {
	return []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin",
		"DISABLE_UPDATES=1", "TERM=dumb"}
}

func TestHarnessProbeClaudeCode(t *testing.T) {
	binary := requireProbe(t, "claude")
	const model = "claude-sonnet-5"
	for _, variant := range []struct {
		name string
		env  func(base, token string) []string
	}{
		{"auth-token-only", func(base, token string) []string {
			return []string{"ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_AUTH_TOKEN=" + token}
		}},
		{"api-key-only", func(base, token string) []string {
			return []string{"ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_API_KEY=" + token}
		}},
		{"worker-config", func(base, token string) []string { // what tooladapter writes
			return []string{"ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_AUTH_TOKEN=" + token, "ANTHROPIC_API_KEY=" + token}
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			log, events := &probeLog{}, &recorder{}
			front := probeGateway(t, "anthropic", model, log, events)
			home, work := t.TempDir(), t.TempDir()
			env := append(baseEnv(home), variant.env(front.URL+"/anthropic", probeToken(t))...)
			out := runProbe(t, binary, work, env, "--bare", "--settings", `{"availableModels":["`+model+`"],"fallbackModel":[]}`,
				"--model", model, "--print", "--output-format", "stream-json", "--verbose", "--", "Reply with probe-ok")
			gw, up := log.snapshot()
			t.Logf("gateway saw:\n  %s\nupstream saw:\n  %s", strings.Join(gw, "\n  "), strings.Join(up, "\n  "))
			if !strings.Contains(out, "probe-ok") || len(up) == 0 {
				t.Errorf("Claude Code did not complete through the gateway")
			}
			for _, line := range up {
				if !strings.Contains(line, "auth=x-api-key:provider-key") {
					t.Errorf("upstream did not receive only the provider key: %s", line)
				}
			}
			if e := events.last(t); !e.Usage.Reported || e.Usage.Output != 7 {
				t.Errorf("metering: %+v", e.Usage)
			}
		})
	}
}

func TestHarnessProbeCodex(t *testing.T) {
	binary := requireProbe(t, "codex")
	const model = "gpt-6-luna"
	quote := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	for _, variant := range []struct {
		name string
		env  func(token string) []string
		args func(base string) []string
	}{
		{"env OPENAI_BASE_URL + OPENAI_API_KEY",
			func(token string) []string { return []string{"OPENAI_API_KEY=" + token} },
			func(base string) []string { return nil }},
		{"config openai_base_url + OPENAI_API_KEY",
			func(token string) []string { return []string{"OPENAI_API_KEY=" + token} },
			func(base string) []string { return []string{"--config", "openai_base_url=" + quote(base)} }},
		{"config openai_base_url + CODEX_API_KEY",
			func(token string) []string { return []string{"CODEX_API_KEY=" + token} },
			func(base string) []string { return []string{"--config", "openai_base_url=" + quote(base)} }},
		{"config openai_base_url + CODEX_API_KEY, openai websockets off",
			func(token string) []string { return []string{"CODEX_API_KEY=" + token} },
			func(base string) []string {
				return []string{"--config", "openai_base_url=" + quote(base), "--config", "model_providers.openai.supports_websockets=false"}
			}},
		// Exactly what tooladapter.codexGatewayArgs writes (asserted there).
		{"worker-config: gateway model provider, no websockets",
			func(token string) []string { return []string{"OPENAI_API_KEY=" + token} },
			func(base string) []string {
				return []string{"--config", `model_provider="blaxsmith-gateway"`, "--config",
					`model_providers.blaxsmith-gateway={name="Blaxsmith model gateway",base_url=` + quote(base) +
						`,env_key="OPENAI_API_KEY",wire_api="responses",supports_websockets=false}`}
			}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			log, events := &probeLog{}, &recorder{}
			front := probeGateway(t, "openai", model, log, events)
			home, work := t.TempDir(), t.TempDir()
			if out, err := exec.Command("git", "-C", work, "init", "-q").CombinedOutput(); err != nil {
				t.Fatalf("git init: %s", out)
			}
			base := front.URL + "/openai/v1"
			env := append(append(baseEnv(home), "CODEX_HOME="+filepath.Join(home, ".codex"), "OPENAI_BASE_URL="+base),
				variant.env(probeToken(t))...)
			_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
			args := []string{"exec", "--json", "--ignore-user-config", "--disable", "multi_agent", "--disable", "apps",
				"--disable", "plugins", "--sandbox", "workspace-write", "--model", model, "--config", `approval_policy="never"`,
				"--config", `model_reasoning_effort="medium"`, "--config", `web_search="disabled"`, "--config", "skills.bundled.enabled=false",
				"--skip-git-repo-check"}
			args = append(append(args, variant.args(base)...), "Reply with probe-ok")
			ctx, cancel := context.WithTimeout(tenant.System(t.Context()), 40*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Dir, cmd.Env, cmd.Stdin = work, env, strings.NewReader("")
			raw, err := cmd.CombinedOutput()
			out := string(raw)
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if len(lines) > 6 {
				lines = lines[len(lines)-6:]
			}
			t.Logf("codex exit=%v\n%s", err, strings.Join(lines, "\n"))
			gw, up := log.snapshot()
			t.Logf("gateway saw:\n  %s\nupstream saw:\n  %s", strings.Join(gw, "\n  "), strings.Join(up, "\n  "))
			ok := strings.Contains(out, `"text":"probe-ok"`) && len(up) > 0
			t.Logf("RESULT %s: completed through gateway=%t reached api.openai.com=%t", variant.name, ok, strings.Contains(out, "api.openai.com"))
			if strings.HasPrefix(variant.name, "worker-config") {
				if !ok || strings.Contains(out, "api.openai.com") {
					t.Errorf("the worker's Codex config did not complete through the gateway")
				}
				for _, line := range gw {
					if strings.HasPrefix(line, "GET ") {
						t.Errorf("WebSocket upgrade attempted: %s", line)
					}
				}
			}
			for _, line := range up {
				if !strings.Contains(line, "auth=bearer:provider-key") {
					t.Errorf("upstream credential: %s", line)
				}
			}
			if ok {
				if e := events.last(t); !e.Usage.Reported || e.Usage.CacheRead != 10 || e.Usage.Input != 20 {
					t.Errorf("metering: %+v", e.Usage)
				}
			}
		})
	}
}

func TestHarnessProbeOpenCode(t *testing.T) {
	binary := requireProbe(t, "opencode")
	const model = "probe-model"
	log, events := &probeLog{}, &recorder{}
	front := probeGateway(t, "opencode", model, log, events)
	home, work := t.TempDir(), t.TempDir()
	config := filepath.Join(home, ".config", "opencode")
	_ = os.MkdirAll(config, 0o700)
	// The config tooladapter writes for a brokered attempt (openCodeGatewayProvider),
	// plus a model entry so an unlisted probe model resolves.
	settings := map[string]any{"update": "disable", "model": "opencode/" + model,
		"provider": map[string]any{"opencode": map[string]any{
			"options": map[string]any{"baseURL": front.URL + "/opencode/v1", "apiKey": "{env:OPENCODE_API_KEY}"},
			"models":  map[string]any{model: map[string]any{"name": "Probe"}}}}}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(config, "opencode.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	env := append(baseEnv(home), "OPENCODE_API_KEY="+probeToken(t))
	out := runProbe(t, binary, work, env, "run", "--format", "json", "--model", "opencode/"+model, "Reply with probe-ok")
	gw, up := log.snapshot()
	t.Logf("gateway saw:\n  %s\nupstream saw:\n  %s", strings.Join(gw, "\n  "), strings.Join(up, "\n  "))
	if !strings.Contains(out, "probe-ok") || len(up) == 0 {
		t.Errorf("OpenCode did not complete through the gateway")
	}
	for _, line := range up {
		if !strings.Contains(line, "provider-key") {
			t.Errorf("upstream credential: %s", line)
		}
	}
	_ = events
}
