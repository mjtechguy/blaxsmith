package tooladapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Sample lines follow the pinned CLIs' JSON event shapes (Claude Code 2.1.281
// stream-json, codex-cli 0.156.1 exec --json, opencode v2.0.14 run --format json).
var activitySamples = map[string][]string{
	"claude-code": {
		`{"type":"system","subtype":"init","session_id":"s","model":"claude-opus-5-5"}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"I'll run the tests first."}]},"session_id":"s"}`,
		`{"type":"assistant","message":{"id":"msg_1","content":[{"type":"tool_use","id":"toolu_todo","name":"TodoWrite","input":{"todos":[{"content":"Run tests","status":"in_progress","activeForm":"Running tests"},{"content":"Fix CSV header","status":"pending","activeForm":"Fixing"}]}}]},"session_id":"s"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_todo","content":"Todos have been modified successfully."}]},"session_id":"s"}`,
		`{"type":"assistant","message":{"id":"msg_2","content":[{"type":"tool_use","id":"toolu_bash","name":"Bash","input":{"command":"go test ./internal/export/...","description":"Run export tests"}}]},"session_id":"s"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_bash","is_error":true,"content":"Exit code 1\n--- FAIL: TestExportCSV"}]},"session_id":"s"}`,
		`{"type":"assistant","message":{"id":"msg_3","content":[{"type":"tool_use","id":"toolu_read","name":"Read","input":{"file_path":"/workspace/source/internal/export/csv.go"}}]},"session_id":"s"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_read","content":[{"type":"text","text":"package export"}]}]},"session_id":"s"}`,
		`{"type":"assistant","message":{"id":"msg_4","content":[{"type":"tool_use","id":"toolu_edit","name":"Edit","input":{"file_path":"/workspace/source/internal/export/csv.go","old_string":"a\nb","new_string":"a\nb\nc"}}]},"session_id":"s"}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"toolu_edit","content":"The file has been updated."}]},"session_id":"s"}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"Done","session_id":"s"}`,
	},
	"codex": {
		`{"type":"thread.started","thread_id":"t"}`,
		`{"type":"item.started","item":{"id":"item_0","type":"todo_list","items":[{"text":"Run tests","completed":false},{"text":"Fix header","completed":false}]}}`,
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc 'go test ./...'","aggregated_output":"","exit_code":null,"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"bash -lc 'go test ./...'","aggregated_output":"FAIL","exit_code":1,"status":"failed"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"file_change","changes":[{"path":"/workspace/source/internal/export/csv.go","kind":"update"},{"path":"/workspace/source/internal/export/csv_test.go","kind":"add"}],"status":"completed"}}`,
		`{"type":"item.updated","item":{"id":"item_0","type":"todo_list","items":[{"text":"Run tests","completed":true},{"text":"Fix header","completed":false}]}}`,
		`{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"query":"csv"},"status":"in_progress"}}`,
		`{"type":"item.completed","item":{"id":"item_3","type":"mcp_tool_call","server":"docs","tool":"search","arguments":{"query":"csv"},"status":"completed"}}`,
		`{"type":"item.completed","item":{"id":"item_4","type":"agent_message","text":"Fixed the CSV header."}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	},
	"opencode": {
		`{"type":"step_start","sessionID":"ses_1","part":{"id":"prt_0","type":"step-start"}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"id":"prt_1","callID":"call_todo","type":"tool","tool":"todowrite","state":{"status":"completed","input":{"todos":[{"content":"Run tests","status":"completed","priority":"high","id":"1"},{"content":"Fix header","status":"in_progress","priority":"high","id":"2"}]},"time":{"start":1790259614000,"end":1790259614010}}}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"id":"prt_2","callID":"call_bash","type":"tool","tool":"bash","state":{"status":"completed","input":{"command":"go test ./...","description":"Run tests"},"output":"ok","metadata":{"output":"ok","exit":0},"time":{"start":1790259614000,"end":1790259616500}}}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"id":"prt_3","callID":"call_edit","type":"tool","tool":"edit","state":{"status":"completed","input":{"filePath":"/workspace/source/internal/export/csv.go","oldString":"a","newString":"a\nb"},"metadata":{"diff":"@@","filediff":{"file":"/workspace/source/internal/export/csv.go","additions":4,"deletions":2}},"time":{"start":1790259617000,"end":1790259617100}}}}`,
		`{"type":"tool_use","sessionID":"ses_1","part":{"id":"prt_4","callID":"call_grep","type":"tool","tool":"grep","state":{"status":"error","input":{"pattern":"created_at"},"error":"bad regex","time":{"start":1790259618000,"end":1790259618020}}}}`,
		`{"type":"text","sessionID":"ses_1","part":{"id":"prt_5","type":"text","text":"Header fixed.","time":{"start":1,"end":2}}}`,
	},
}

func normalizeAll(t *testing.T, harness string) []map[string]any {
	t.Helper()
	a := newActivity("/workspace/source", time.Hour, nil)
	clock := time.Unix(1_790_000_000, 0)
	a.now = func() time.Time { clock = clock.Add(250 * time.Millisecond); return clock }
	var out []map[string]any
	for _, line := range activitySamples[harness] {
		for _, e := range a.normalize([]byte(line)) {
			var m map[string]any
			if err := json.Unmarshal(boundPayload(e), &m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}

func summary(events []map[string]any) string {
	var parts []string
	for _, e := range events {
		s := e["type"].(string)
		for _, k := range []string{"id", "tool", "cmd", "input", "path", "kind", "status", "exit", "added", "removed", "duration_ms", "text"} {
			if v, ok := e[k]; ok {
				data, _ := json.Marshal(v)
				s += " " + k + "=" + string(data)
			}
		}
		if items, ok := e["items"].([]any); ok {
			for _, it := range items {
				m := it.(map[string]any)
				s += " [" + m["status"].(string) + "] " + m["text"].(string)
			}
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

func TestActivityNormalizesEachHarness(t *testing.T) {
	want := map[string]string{
		"claude-code": `assistant.message id="msg_1" text="I'll run the tests first."
plan.updated id="plan" [in_progress] Run tests [pending] Fix CSV header
command id="toolu_bash" cmd="go test ./internal/export/..." status="running"
command id="toolu_bash" cmd="go test ./internal/export/..." status="failed" exit=1 duration_ms=250
tool.started id="toolu_read" tool="Read" input="/workspace/source/internal/export/csv.go" status="running"
tool.completed id="toolu_read" tool="Read" input="/workspace/source/internal/export/csv.go" status="completed" duration_ms=250
file.changed id="toolu_edit" tool="Edit" path="internal/export/csv.go" status="completed" added=3 removed=2`,
		"codex": `plan.updated id="plan" [in_progress] Run tests [pending] Fix header
command id="item_1" cmd="go test ./..." status="running"
command id="item_1" cmd="go test ./..." status="failed" exit=1 duration_ms=250
file.changed id="item_2:0" path="internal/export/csv.go" kind="update" status="completed"
file.changed id="item_2:1" path="internal/export/csv_test.go" kind="add" status="completed"
plan.updated id="plan" [completed] Run tests [in_progress] Fix header
tool.started id="item_3" tool="docs/search" input="csv" status="running"
tool.completed id="item_3" tool="docs/search" input="csv" status="completed" duration_ms=250
assistant.message id="item_4" text="Fixed the CSV header."`,
		"opencode": `plan.updated id="plan" [completed] Run tests [in_progress] Fix header
command id="call_bash" cmd="go test ./..." status="completed" exit=0 duration_ms=2500
file.changed id="call_edit" tool="edit" path="internal/export/csv.go" status="completed" added=4 removed=2
tool.completed id="call_grep" tool="grep" input="created_at" status="failed" duration_ms=20
assistant.message id="prt_5" text="Header fixed."`,
	}
	for harness, expected := range want {
		if got := summary(normalizeAll(t, harness)); got != expected {
			t.Errorf("%s:\n got:\n%s\nwant:\n%s", harness, got, expected)
		}
	}
}

func TestActivityPayloadsAreBoundedAndRedacted(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-leased-secret-0123456789")
	a := newActivity("", time.Hour, nil)
	huge := strings.Repeat("ü", 5000)
	todos := make([]any, 80)
	for i := range todos {
		todos[i] = map[string]any{"content": strings.Repeat("x", 400), "status": "pending"}
	}
	lines := []string{
		`{"type":"assistant","message":{"id":"m","content":[{"type":"text","text":"key sk-ant-leased-secret-0123456789 ` + huge + `"}]}}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"tool_use","id":"b","name":"Bash","input":{"command":"echo sk-ant-leased-secret-0123456789 ` + huge + `"}}]}}`,
	}
	plan, _ := json.Marshal(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "tool_use", "id": "p", "name": "TodoWrite", "input": map[string]any{"todos": todos}}}}})
	lines = append(lines, string(plan))
	for _, line := range lines {
		for _, e := range a.normalize([]byte(line)) {
			data := boundPayload(e)
			if len(data) > activityMaxBytes {
				t.Fatalf("%s payload is %d bytes", e["type"], len(data))
			}
			if strings.Contains(string(data), "leased-secret") {
				t.Fatalf("leased key reached a payload: %s", data)
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil || m["truncated"] != true {
				t.Fatalf("truncation not marked: %.200s", data)
			}
			if items, ok := m["items"].([]any); ok && (len(items) == 0 || len(items) > planMaxItems) {
				t.Fatalf("plan items not capped: %d", len(items))
			}
		}
	}
}

type recorder struct {
	mu   sync.Mutex
	got  []string
	seen chan struct{}
}

func (r *recorder) write(p []byte) {
	r.mu.Lock()
	r.got = append(r.got, string(p))
	r.mu.Unlock()
	select {
	case r.seen <- struct{}{}:
	default:
	}
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.got...)
}

func TestCoalescerHoldsUpdatesAndLetsFinalWin(t *testing.T) {
	r := &recorder{seen: make(chan struct{}, 16)}
	c := &coalescer{window: 40 * time.Millisecond, write: r.write, pending: map[string]*held{}}
	// A fast call: started then completed inside the window writes only the final record.
	c.emit("tool\x00a", []byte("a-started"), false)
	c.emit("tool\x00a", []byte("a-completed"), true)
	// A burst of plan updates writes only the latest, once, after the window.
	for _, p := range []string{"plan-1", "plan-2", "plan-3"} {
		c.emit("plan\x00plan", []byte(p), false)
	}
	if got := r.snapshot(); len(got) != 1 || got[0] != "a-completed" {
		t.Fatalf("before window: %q", got)
	}
	select {
	case <-r.seen:
	case <-time.After(time.Second):
	}
	eventually := time.Now().Add(time.Second)
	for len(r.snapshot()) < 2 && time.Now().Before(eventually) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.snapshot(); len(got) != 2 || got[1] != "plan-3" {
		t.Fatalf("after window: %q", got)
	}
	// A long-running command shows as running once, then its final record.
	c.emit("command\x00b", []byte("b-running"), false)
	c.emit("command\x00b", []byte("b-running-2"), false)
	c.flush()
	c.emit("command\x00b", []byte("b-done"), true)
	time.Sleep(60 * time.Millisecond) // a stopped timer must not write again
	if got := r.snapshot(); strings.Join(got[2:], ",") != "b-running-2,b-done" {
		t.Fatalf("flush and final: %q", got)
	}
}

func TestPaneActivityWritesWatchLog(t *testing.T) {
	t.Setenv("BLAXSMITH_STATE_DIR", t.TempDir())
	a := paneActivity("/workspace/source")
	for _, line := range activitySamples["codex"] {
		a.Observe([]byte(line))
	}
	a.Close()
	var types []string
	for _, entry := range sequenced("log") {
		data, err := os.ReadFile(entry.path)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Seq   int64          `json:"seq"`
			Event map[string]any `json:"event"`
		}
		if err := json.Unmarshal(data, &record); err != nil || record.Seq != entry.seq {
			t.Fatalf("watch record %s: %v", filepath.Base(entry.path), err)
		}
		types = append(types, record.Event["type"].(string))
	}
	// Running commands and the first plan are held, then superseded or flushed.
	if got := strings.Join(types, ","); got != "command,file.changed,file.changed,tool.completed,assistant.message,plan.updated" {
		t.Fatalf("watch log types: %s", got)
	}
}

func TestActivityRedactsEveryLeasedKey(t *testing.T) {
	for _, name := range credentialEnvNames {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "leased-"+name+"-0123456789")
			if got := redactText("echo leased-" + name + "-0123456789"); strings.Contains(got, "leased-") {
				t.Fatalf("%s not redacted: %q", name, got)
			}
		})
	}
}
