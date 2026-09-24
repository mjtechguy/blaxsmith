package tooladapter

// Structured agent activity for the stage work log. While pane renders each
// harness JSON line for the terminal, it also normalizes the line into a few
// bounded `bx event` records (docs/interactive-sessions.md, "Agent activity"):
//
//	tool.started / tool.completed  {id, tool, input, status, duration_ms?}
//	command                        {id, cmd, status, exit?, duration_ms?}
//	file.changed                   {id, path, kind?, added?, removed?, status}
//	plan.updated                   {id:"plan", items:[{text, status}]}
//	assistant.message              {id, text}
//
// Every record carries an item id; a later record for the same (family, id)
// replaces the earlier one in the UI. Shape and coalescing ideas follow
// t3code's provider runtime events and ThreadLiveEventCoalescer (MIT).

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	activityWindow   = 500 * time.Millisecond // non-final updates per item are held this long
	activityMaxBytes = 4 << 10                // well under the 8 KiB workflow_events payload cap
	activityMaxOpen  = 256                    // tracked in-flight tool calls
	planMaxItems     = 30
)

type activityEvent map[string]any

// family groups the record types that describe the same item.
func (e activityEvent) family() string {
	t, _ := e["type"].(string)
	if strings.HasPrefix(t, "tool.") {
		return "tool"
	}
	return t
}

// final records are emitted at once; the rest wait for the coalescing window.
func (e activityEvent) final() bool {
	switch e["type"] {
	case "tool.completed", "file.changed", "assistant.message":
		return true
	case "command":
		return e["status"] != "running"
	}
	return false
}

type openTool struct {
	kind    string // tool, command, file, plan
	name    string
	input   string
	path    string
	added   int
	removed int
	started time.Time
}

// activity normalizes harness lines and hands bounded payloads to a coalescer.
type activity struct {
	dir  string // workspace root; paths inside it are shown relative
	now  func() time.Time
	open map[string]openTool
	out  *coalescer
}

func newActivity(dir string, window time.Duration, write func([]byte)) *activity {
	return &activity{dir: dir, now: time.Now, open: map[string]openTool{},
		out: &coalescer{window: window, write: write, pending: map[string]*held{}}}
}

// paneActivity writes records into the guest watch log that `bx watch` streams.
func paneActivity(dir string) *activity {
	return newActivity(dir, activityWindow, func(payload []byte) {
		_ = appendRecord("log", "event", payload)
	})
}

func (a *activity) Observe(line []byte) {
	for _, e := range a.normalize(line) {
		id, _ := e["id"].(string)
		a.out.emit(e.family()+"\x00"+id, boundPayload(e), e.final())
	}
}

func (a *activity) Close() { a.out.flush() }

func (a *activity) normalize(line []byte) []activityEvent {
	var e struct {
		Type    string          `json:"type"`
		Message json.RawMessage `json:"message"`
		Item    json.RawMessage `json:"item"`
		Part    json.RawMessage `json:"part"`
	}
	if json.Unmarshal(line, &e) != nil {
		return nil
	}
	switch e.Type {
	case "assistant", "user": // Claude Code stream-json
		return a.claude(e.Type, e.Message)
	case "item.started", "item.updated", "item.completed": // Codex exec --json
		return a.codex(e.Type, e.Item)
	case "tool_use", "text": // OpenCode run --format json
		return a.opencode(e.Type, e.Part)
	}
	return nil
}

// Claude Code: tool_use blocks in assistant messages, tool_result blocks in
// user messages. The result names only the tool_use id, so calls are tracked.
func (a *activity) claude(kind string, raw json.RawMessage) []activityEvent {
	var m struct {
		ID      string `json:"id"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
			ToolUseID string          `json:"tool_use_id"`
			IsError   bool            `json:"is_error"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []activityEvent
	for i, c := range m.Content {
		switch {
		case kind == "assistant" && c.Type == "text" && strings.TrimSpace(c.Text) != "":
			id := m.ID
			if id == "" {
				id = fmt.Sprintf("text-%d", i)
			}
			out = append(out, activityEvent{"type": "assistant.message", "id": id, "text": c.Text})
		case kind == "assistant" && c.Type == "tool_use" && c.ID != "":
			var in map[string]any
			_ = json.Unmarshal(c.Input, &in)
			if e := a.start(c.ID, c.Name, in); e != nil {
				out = append(out, e)
			}
		case kind == "user" && c.Type == "tool_result":
			text := resultText(c.Content)
			exit := -1
			if c.IsError {
				if n, ok := exitCodePrefix(text); ok {
					exit = n
				}
			} else {
				exit = 0
			}
			if e := a.finish(c.ToolUseID, !c.IsError, exit, 0); e != nil {
				out = append(out, e)
			}
		}
	}
	return out
}

// Codex: items with a stable id through started/updated/completed.
func (a *activity) codex(kind string, raw json.RawMessage) []activityEvent {
	var it struct {
		ID       string          `json:"id"`
		Type     string          `json:"type"`
		Text     string          `json:"text"`
		Command  string          `json:"command"`
		ExitCode *int            `json:"exit_code"`
		Status   string          `json:"status"`
		Server   string          `json:"server"`
		Tool     string          `json:"tool"`
		Args     json.RawMessage `json:"arguments"`
		Query    string          `json:"query"`
		Changes  []struct {
			Path string `json:"path"`
			Kind string `json:"kind"`
		} `json:"changes"`
		Items []struct {
			Text      string `json:"text"`
			Completed bool   `json:"completed"`
		} `json:"items"`
	}
	if json.Unmarshal(raw, &it) != nil || it.ID == "" {
		return nil
	}
	done := kind == "item.completed"
	switch it.Type {
	case "agent_message":
		if done && strings.TrimSpace(it.Text) != "" {
			return []activityEvent{{"type": "assistant.message", "id": it.ID, "text": it.Text}}
		}
	case "command_execution":
		if !done {
			return one(a.start(it.ID, "shell", map[string]any{"command": unwrapShell(it.Command)}))
		}
		a.ensure(it.ID, "shell", map[string]any{"command": unwrapShell(it.Command)})
		exit := -1
		if it.ExitCode != nil {
			exit = *it.ExitCode
		}
		return one(a.finish(it.ID, it.Status != "failed" && exit <= 0, exit, 0))
	case "file_change":
		if !done {
			return nil
		}
		var out []activityEvent
		for i, c := range it.Changes {
			e := activityEvent{"type": "file.changed", "id": fmt.Sprintf("%s:%d", it.ID, i), "path": a.rel(c.Path),
				"status": "completed"}
			if it.Status == "failed" {
				e["status"] = "failed"
			}
			if c.Kind != "" {
				e["kind"] = c.Kind
			}
			out = append(out, e)
		}
		return out
	case "todo_list":
		items := make([]map[string]any, 0, len(it.Items))
		pendingSeen := false
		for _, item := range it.Items {
			status := "completed"
			if !item.Completed {
				// Codex reports done/not-done; the first open item is the one in progress.
				status = "pending"
				if !pendingSeen {
					status, pendingSeen = "in_progress", true
				}
			}
			items = append(items, map[string]any{"text": item.Text, "status": status})
		}
		return []activityEvent{planEvent(items)}
	case "mcp_tool_call", "web_search":
		name, in := it.Server+"/"+it.Tool, map[string]any{}
		_ = json.Unmarshal(it.Args, &in)
		if it.Type == "web_search" {
			name, in = "web_search", map[string]any{"query": it.Query}
		}
		if !done {
			return one(a.start(it.ID, name, in))
		}
		a.ensure(it.ID, name, in)
		return one(a.finish(it.ID, it.Status != "failed", -1, 0))
	}
	return nil
}

// OpenCode: one tool part per call, re-sent as its state advances.
func (a *activity) opencode(kind string, raw json.RawMessage) []activityEvent {
	var p struct {
		ID     string `json:"id"`
		CallID string `json:"callID"`
		Text   string `json:"text"`
		Tool   string `json:"tool"`
		State  struct {
			Status   string         `json:"status"`
			Input    map[string]any `json:"input"`
			Metadata struct {
				Exit     *int `json:"exit"`
				Todos    []map[string]any
				FileDiff *struct {
					Additions int `json:"additions"`
					Deletions int `json:"deletions"`
				} `json:"filediff"`
			} `json:"metadata"`
			Time struct {
				Start int64 `json:"start"`
				End   int64 `json:"end"`
			} `json:"time"`
		} `json:"state"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	if kind == "text" {
		if p.ID == "" || strings.TrimSpace(p.Text) == "" {
			return nil
		}
		return []activityEvent{{"type": "assistant.message", "id": p.ID, "text": p.Text}}
	}
	id := firstNonEmpty(p.CallID, p.ID)
	if id == "" || p.Tool == "" {
		return nil
	}
	in := p.State.Input
	if strings.EqualFold(p.Tool, "todowrite") && in["todos"] == nil && p.State.Metadata.Todos != nil {
		in = map[string]any{"todos": anySlice(p.State.Metadata.Todos)}
	}
	var out []activityEvent
	_, known := a.open[id]
	if !known {
		if e := a.start(id, p.Tool, in); e != nil {
			out = append(out, e)
		}
	}
	if p.State.Status != "completed" && p.State.Status != "error" {
		return out
	}
	if d := p.State.Metadata.FileDiff; d != nil {
		if t, ok := a.open[id]; ok {
			t.added, t.removed = d.Additions, d.Deletions
			a.open[id] = t
		}
	}
	exit := -1
	if p.State.Metadata.Exit != nil {
		exit = *p.State.Metadata.Exit
	}
	var took time.Duration
	if p.State.Time.End > p.State.Time.Start && p.State.Time.Start > 0 {
		took = time.Duration(p.State.Time.End-p.State.Time.Start) * time.Millisecond
	}
	ok := p.State.Status == "completed" && exit <= 0
	if e := a.finish(id, ok, exit, took); e != nil {
		// A call first seen already finished reports only its final record.
		if !known && len(out) == 1 && out[0].family() == e.family() {
			out = out[:0]
		}
		out = append(out, e)
	}
	return out
}

// start records a call and returns its opening record (plan updates are
// complete at the call; edits report only when they finish).
func (a *activity) start(id, name string, in map[string]any) activityEvent {
	t := classify(name, in)
	t.started = a.now()
	switch t.kind {
	case "plan":
		a.track(id, t)
		return planEvent(todoItems(in))
	case "file":
		t.path = a.rel(t.path)
		a.track(id, t)
		return nil
	case "command":
		a.track(id, t)
		return activityEvent{"type": "command", "id": id, "cmd": t.input, "status": "running"}
	}
	a.track(id, t)
	return activityEvent{"type": "tool.started", "id": id, "tool": t.name, "input": t.input, "status": "running"}
}

// ensure tracks a call first seen at completion (no started line).
func (a *activity) ensure(id, name string, in map[string]any) {
	if _, ok := a.open[id]; !ok {
		t := classify(name, in)
		t.started = a.now()
		a.track(id, t)
	}
}

func (a *activity) track(id string, t openTool) {
	if len(a.open) >= activityMaxOpen {
		oldest := ""
		for k, v := range a.open {
			if oldest == "" || v.started.Before(a.open[oldest].started) {
				oldest = k
			}
		}
		delete(a.open, oldest)
	}
	a.open[id] = t
}

// finish closes a call. exit < 0 means unknown; took == 0 measures in-guest.
func (a *activity) finish(id string, ok bool, exit int, took time.Duration) activityEvent {
	t, found := a.open[id]
	if !found {
		return nil
	}
	delete(a.open, id)
	if took == 0 {
		took = a.now().Sub(t.started)
	}
	status := "completed"
	if !ok {
		status = "failed"
	}
	ms := took.Milliseconds()
	switch t.kind {
	case "plan":
		return nil
	case "file":
		e := activityEvent{"type": "file.changed", "id": id, "path": t.path, "status": status, "tool": t.name}
		if t.added > 0 || t.removed > 0 {
			e["added"], e["removed"] = t.added, t.removed
		}
		return e
	case "command":
		e := activityEvent{"type": "command", "id": id, "cmd": t.input, "status": status, "duration_ms": ms}
		if exit >= 0 {
			e["exit"] = exit
		}
		return e
	}
	return activityEvent{"type": "tool.completed", "id": id, "tool": t.name, "input": t.input, "status": status, "duration_ms": ms}
}

// classify maps a harness tool name onto the work-log families.
func classify(name string, in map[string]any) openTool {
	str := func(keys ...string) string {
		for _, k := range keys {
			if s, ok := in[k].(string); ok && s != "" {
				return s
			}
		}
		return ""
	}
	lower := strings.ToLower(name)
	t := openTool{kind: "tool", name: name}
	switch {
	case strings.Contains(lower, "todowrite") || lower == "update_plan":
		t.kind = "plan"
	case lower == "bash" || lower == "shell" || lower == "exec_command":
		t.kind, t.input = "command", str("command", "cmd")
	case lower == "edit" || lower == "multiedit" || lower == "write" || lower == "notebookedit" || lower == "patch":
		t.kind, t.path = "file", str("file_path", "filePath", "notebook_path", "path")
		switch lower {
		case "write":
			t.added = lines(str("content"))
		case "multiedit":
			edits, _ := in["edits"].([]any)
			for _, raw := range edits {
				if e, ok := raw.(map[string]any); ok {
					n, _ := e["new_string"].(string)
					o, _ := e["old_string"].(string)
					t.added, t.removed = t.added+lines(n), t.removed+lines(o)
				}
			}
		default:
			t.added, t.removed = lines(str("new_string", "newString")), lines(str("old_string", "oldString"))
		}
	default:
		t.input = str("file_path", "filePath", "path", "pattern", "query", "url", "description", "command", "prompt")
		if t.input == "" && len(in) > 0 {
			data, _ := json.Marshal(in)
			t.input = string(data)
		}
	}
	return t
}

func planEvent(items []map[string]any) activityEvent {
	return activityEvent{"type": "plan.updated", "id": "plan", "items": items}
}

// todoItems reads Claude TodoWrite / OpenCode todowrite input.
func todoItems(in map[string]any) []map[string]any {
	todos, _ := in["todos"].([]any)
	items := make([]map[string]any, 0, len(todos))
	for _, raw := range todos {
		todo, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		text, _ := todo["content"].(string)
		status, _ := todo["status"].(string)
		switch status {
		case "completed", "in_progress", "cancelled":
		default:
			status = "pending"
		}
		items = append(items, map[string]any{"text": strings.TrimSpace(text), "status": status})
	}
	return items
}

func (a *activity) rel(path string) string {
	if a.dir != "" {
		if r, err := filepath.Rel(a.dir, path); err == nil && !strings.HasPrefix(r, "..") && filepath.IsAbs(path) {
			return r
		}
	}
	return path
}

var shellWrapper = regexp.MustCompile(`^(?:/bin/|/usr/bin/)?(?:ba|z)?sh -l?c (.*)$`)

func unwrapShell(cmd string) string {
	if m := shellWrapper.FindStringSubmatch(cmd); m != nil {
		inner := m[1]
		if len(inner) >= 2 && (inner[0] == '\'' || inner[0] == '"') && inner[len(inner)-1] == inner[0] {
			inner = inner[1 : len(inner)-1]
		}
		return inner
	}
	return cmd
}

var exitPrefix = regexp.MustCompile(`^\s*(?:Error: )?Exit code (\d{1,3})\b`)

func exitCodePrefix(text string) (int, bool) {
	m := exitPrefix.FindStringSubmatch(text)
	if m == nil {
		return 0, false
	}
	var n int
	_, err := fmt.Sscan(m[1], &n)
	return n, err == nil
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct{ Type, Text string }
	_ = json.Unmarshal(raw, &parts)
	for _, p := range parts {
		if p.Type == "text" {
			return p.Text
		}
	}
	return ""
}

func lines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(strings.TrimSuffix(s, "\n"), "\n") + 1
}

func one(e activityEvent) []activityEvent {
	if e == nil {
		return nil
	}
	return []activityEvent{e}
}

func anySlice(in []map[string]any) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// Field caps in runes. Longer values end in "…" and set "truncated": true.
var activityCaps = map[string]int{"text": 1000, "input": 200, "cmd": 500, "path": 300, "tool": 80, "id": 120}

// boundPayload redacts leased keys and caps the record at activityMaxBytes.
func boundPayload(e activityEvent) []byte {
	truncated := false
	for key, limit := range activityCaps {
		if s, ok := e[key].(string); ok {
			s = redactText(s)
			if key == "input" || key == "cmd" {
				s = strings.Join(strings.Fields(s), " ")
			}
			if b, cut := bound(s, limit); cut {
				s, truncated = b, true
			}
			e[key] = s
		}
	}
	if items, ok := e["items"].([]map[string]any); ok {
		if len(items) > planMaxItems {
			items, truncated = items[:planMaxItems], true
		}
		for _, item := range items {
			if s, ok := item["text"].(string); ok {
				b, cut := bound(redactText(s), 160)
				item["text"], truncated = b, truncated || cut
			}
		}
		e["items"] = items
	}
	if truncated {
		e["truncated"] = true
	}
	for {
		data, err := json.Marshal(e)
		if err != nil {
			return []byte(`{"type":"progress","message":"activity record could not be encoded"}`)
		}
		if len(data) <= activityMaxBytes {
			return data
		}
		e["truncated"] = true
		if items, ok := e["items"].([]map[string]any); ok && len(items) > 1 {
			e["items"] = items[:len(items)/2]
			continue
		}
		for key := range activityCaps {
			if s, ok := e[key].(string); ok && utf8.RuneCountInString(s) > 100 {
				e[key], _ = bound(s, 100)
			}
		}
		if data, err := json.Marshal(e); err == nil && len(data) <= activityMaxBytes {
			return data
		}
		return fmt.Appendf(nil, `{"type":%q,"id":"","truncated":true}`, e["type"])
	}
}

func bound(s string, limit int) (string, bool) {
	s = strings.ToValidUTF8(s, "")
	if utf8.RuneCountInString(s) <= limit {
		return s, false
	}
	return string([]rune(s)[:limit]) + "…", true
}

// redactText hides the leased provider keys and Codex tokens, like the
// terminal redactor.
func redactText(s string) string {
	for _, secret := range leasedSecrets() {
		s = strings.ReplaceAll(s, string(secret), "[redacted]")
	}
	return s
}

// coalescer holds non-final records per item for one window so a burst of
// updates writes only the latest; a final record replaces anything held and
// is written at once. At most one non-final record per item per window.
type coalescer struct {
	mu      sync.Mutex
	window  time.Duration
	write   func([]byte)
	pending map[string]*held
}

type held struct {
	payload []byte
	timer   *time.Timer
}

func (c *coalescer) emit(key string, payload []byte, final bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if h := c.pending[key]; h != nil {
		if !final {
			h.payload = payload
			return
		}
		h.timer.Stop()
		delete(c.pending, key)
	}
	if final {
		c.write(payload)
		return
	}
	h := &held{payload: payload}
	c.pending[key] = h
	h.timer = time.AfterFunc(c.window, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.pending[key] == h { // a final record may have replaced it
			delete(c.pending, key)
			c.write(h.payload)
		}
	})
}

// flush writes everything held, in key order, before the pane exits.
func (c *coalescer) flush() {
	c.mu.Lock()
	defer c.mu.Unlock()
	keys := make([]string, 0, len(c.pending))
	for k := range c.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		h := c.pending[k]
		h.timer.Stop()
		delete(c.pending, k)
		c.write(h.payload)
	}
}
