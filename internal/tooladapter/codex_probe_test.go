package tooladapter

import (
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

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// TestHarnessProbeCodexNativeAPIKey runs the real Codex CLI with the worker's
// exec argv against a mock OpenAI upstream and proves that a native_raw
// API-key-only run authenticates: the key must arrive as CODEX_API_KEY
// (nativeCredentialEnv), because Codex 0.156.1 never sends OPENAI_API_KEY for
// its built-in openai provider. The only probe addition is openai_base_url,
// which re-points the built-in provider at the mock. Opt in with
// BLAXSMITH_HARNESS_PROBE=1; skipped when codex is not on PATH.
func TestHarnessProbeCodexNativeAPIKey(t *testing.T) {
	if os.Getenv("BLAXSMITH_HARNESS_PROBE") != "1" {
		t.Skip("set BLAXSMITH_HARNESS_PROBE=1 to run real harness probes")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Skip("codex not installed")
	}
	const key, model = "sk-native-probe-0123456789", "gpt-6-luna"
	for _, variant := range []struct {
		name     string
		variable string
		want     bool
	}{
		{"worker env", nativeCredentialEnv("codex", "openai"), true},
		{"OPENAI_API_KEY only (negative control)", "OPENAI_API_KEY", false},
	} {
		t.Run(variant.name, func(t *testing.T) {
			var mu sync.Mutex
			var auths []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				mu.Lock()
				auths = append(auths, r.Method+" "+r.URL.Path+" auth="+r.Header.Get("Authorization"))
				mu.Unlock()
				if r.Header.Get("Authorization") != "Bearer "+key {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"error":{"message":"missing key","type":"invalid_request_error"}}`)
					return
				}
				if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
					http.NotFound(w, r) // no WebSocket upgrade; Codex falls back to HTTPS streaming
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, e := range []string{
					`{"type":"response.created","sequence_number":0,"response":{"id":"resp_p","object":"response","status":"in_progress","model":"` + model + `","output":[]}}`,
					`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"in_progress","content":[]}}`,
					`{"type":"response.output_text.delta","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"probe-ok"}`,
					`{"type":"response.output_item.done","sequence_number":3,"output_index":0,"item":{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"probe-ok","annotations":[]}]}}`,
					`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_p","object":"response","status":"completed","model":"` + model + `","output":[],"usage":{"input_tokens":3,"output_tokens":1,"total_tokens":4}}}`,
				} {
					var event struct{ Type string }
					_ = json.Unmarshal([]byte(e), &event)
					_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, e)
					w.(http.Flusher).Flush()
				}
			}))
			defer upstream.Close()

			runtime := Runtime{Harness: "codex", Image: "example/tool@sha256:" + strings.Repeat("a", 64), Binary: binary,
				BinarySHA256: strings.Repeat("a", 64), Version: "0.156.1", Supported: []ModelEffort{{Model: model, Effort: "medium"}}}
			in, err := Prepare(runtime, recipe.Profile{Harness: "codex", Model: model, Effort: "medium"}, "Reply with probe-ok", time.Minute, 1<<16)
			if err != nil {
				t.Fatal(err)
			}
			base, _ := json.Marshal(upstream.URL + "/v1")
			args := append(append(append([]string(nil), in.args[:len(in.args)-1]...),
				"--skip-git-repo-check", "--config", "openai_base_url="+string(base)), in.args[len(in.args)-1])
			home, work := t.TempDir(), t.TempDir()
			_ = os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, args...)
			cmd.Dir, cmd.Stdin = work, strings.NewReader("")
			cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "CODEX_HOME=" + filepath.Join(home, ".codex"),
				"PATH=/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin", "DISABLE_UPDATES=1", "TERM=dumb", variant.variable + "=" + key}
			raw, runErr := cmd.CombinedOutput()
			mu.Lock()
			seen := append([]string(nil), auths...)
			mu.Unlock()
			authenticated := false
			for _, line := range seen {
				authenticated = authenticated || strings.HasPrefix(line, "POST ") && strings.HasSuffix(line, "auth=Bearer "+key)
			}
			completed := strings.Contains(string(raw), `"text":"probe-ok"`)
			t.Logf("%s=<key>: exit=%v authenticated=%t completed=%t upstream=%q", variant.variable, runErr, authenticated, completed, seen)
			if variant.want && (!authenticated || !completed) {
				t.Fatalf("native Codex run did not authenticate with %s:\n%s", variant.variable, raw)
			}
			if !variant.want && (authenticated || completed) {
				t.Fatalf("negative control unexpectedly authenticated; OPENAI_API_KEY may work again")
			}
		})
	}
}
