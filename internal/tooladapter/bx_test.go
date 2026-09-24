package tooladapter

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func bx(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errs bytes.Buffer
	code := Bx(args, &out, &errs)
	return code, out.String()
}

func useIXState(t *testing.T) {
	t.Setenv("BLAXSMITH_STATE_DIR", t.TempDir())
	previous := bxPoll
	bxPoll = 10 * time.Millisecond
	t.Cleanup(func() { bxPoll = previous })
}

// ask runs `bx ask` in the background and reports its exit code and stdout.
func ask(t *testing.T, interaction string) <-chan [2]any {
	t.Helper()
	result := make(chan [2]any, 1)
	go func() {
		var out, errs bytes.Buffer
		code := Bx([]string{"ask", "--json", interaction}, &out, &errs)
		result <- [2]any{code, out.String()}
	}()
	return result
}

func waitAsked(t *testing.T, id string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, out := bx(t, "watch", "--once"); strings.Contains(out, `"id":"`+id+`"`) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("interaction %s never appeared", id)
}

func TestBxAskBlocksUntilAnsweredAndFirstAnswerWins(t *testing.T) {
	useIXState(t)
	pending := ask(t, `{"id":"q1","kind":"interview_round","title":"Scope?","options":[{"id":"a","label":"Small"}],"interview":{"round":1,"finalize_option":"done","tags":["scope"]}}`)
	waitAsked(t, "q1")
	select {
	case r := <-pending:
		t.Fatalf("ask returned before an answer: %v", r)
	case <-time.After(50 * time.Millisecond):
	}
	if code, _ := bx(t, "answer", "unknown", "--json", `{"option_ids":["a"]}`); code != bxInvalid {
		t.Fatalf("unknown interaction must exit 2, got %d", code)
	}
	first := `{"interaction_id":"q1","option_ids":["a"],"text":"keep it small","answered_by":"u1","at":"2026-09-24T00:00:00Z"}`
	if code, _ := bx(t, "answer", "q1", "--json", first); code != bxOK {
		t.Fatalf("answer: %d", code)
	}
	r := <-pending
	if r[0] != bxOK || strings.TrimSpace(r[1].(string)) != first {
		t.Fatalf("ask must print the answer: %v", r)
	}
	// A second, different answer is rejected (the first stays) but exits 0 so
	// platform redelivery is harmless; the same answer again is a no-op.
	if code, _ := bx(t, "answer", "q1", "--json", `{"option_ids":["b"]}`); code != bxOK {
		t.Fatalf("double answer exit: %d", code)
	}
	if code, _ := bx(t, "answer", "q1", "--cancel"); code != bxOK {
		t.Fatalf("late cancel exit: %d", code)
	}
	if code, out := bx(t, "ask", "--json", `{"id":"q1","kind":"interview_round"}`); code != bxOK || strings.TrimSpace(out) != first {
		t.Fatalf("re-ask after a shell timeout must return the first answer: %d %q", code, out)
	}
	if code, _ := bx(t, "ask", "--json", `{"id":"bad id","kind":"question"}`); code != bxInvalid {
		t.Fatalf("invalid id must be rejected: %d", code)
	}
}

func TestBxCancelGivesExit3(t *testing.T) {
	useIXState(t)
	pending := ask(t, `{"id":"gate-1","kind":"approval","title":"Proceed?"}`)
	waitAsked(t, "gate-1")
	if code, _ := bx(t, "answer", "gate-1", "--cancel"); code != bxOK {
		t.Fatalf("cancel: %d", code)
	}
	if r := <-pending; r[0] != bxCancelled {
		t.Fatalf("cancelled ask must exit 3: %v", r)
	}
	_, out := bx(t, "watch", "--once")
	if !strings.Contains(out, `"cancelled":"gate-1"`) {
		t.Fatalf("watch must report the cancellation: %q", out)
	}
}

func TestBxWatchResumesFromCursorWithoutDuplicates(t *testing.T) {
	useIXState(t)
	for _, event := range []string{`{"type":"phase","name":"plan"}`, `{"type":"cycle","n":1,"max":3}`, `{"type":"verdict","status":"fail"}`} {
		if code, _ := bx(t, "event", "--json", event); code != bxOK {
			t.Fatalf("event %s: %d", event, code)
		}
	}
	if code, _ := bx(t, "event", "--json", `{"type":"verdict","status":"maybe"}`); code != bxInvalid {
		t.Fatalf("invalid verdict must be rejected: %d", code)
	}
	_, all := bx(t, "watch", "--once")
	lines := strings.Split(strings.TrimSpace(all), "\n")
	if len(lines) != 3 || lines[0] != `{"seq":1,"event":{"type":"phase","name":"plan"}}` {
		t.Fatalf("watch output: %q", all)
	}
	_, rest := bx(t, "watch", "--cursor", "2", "--once")
	if rest != lines[2]+"\n" {
		t.Fatalf("cursor 2 must yield only seq 3: %q", rest)
	}
	if lastVerdict() != "fail" {
		t.Fatalf("verdict: %q", lastVerdict())
	}
	if code, _ := bx(t, "event", "--json", `{"type":"verdict","status":"pass"}`); code != bxOK || lastVerdict() != "pass" {
		t.Fatalf("latest verdict wins: %q", lastVerdict())
	}
	_, next := bx(t, "watch", "--cursor", "3", "--once")
	var record struct {
		Seq   int64           `json:"seq"`
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal([]byte(next), &record); err != nil || record.Seq != 4 {
		t.Fatalf("next record: %q %v", next, err)
	}
}

func TestBxSteerDedupesAndInboxConsumes(t *testing.T) {
	useIXState(t)
	halt := `{"id":"s-1","kind":"halt","reason":"enough"}`
	for i := 0; i < 2; i++ {
		if code, _ := bx(t, "steer", "--json", halt); code != bxOK {
			t.Fatalf("steer: %d", code)
		}
	}
	if code, _ := bx(t, "steer", "--json", `{"id":"s-2","kind":"set_max_cycles","n":5}`); code != bxOK {
		t.Fatalf("steer: %d", code)
	}
	if code, _ := bx(t, "steer", "--json", `{"id":"s-3","kind":"reboot"}`); code != bxInvalid {
		t.Fatalf("unknown steer kind must be rejected: %d", code)
	}
	_, out := bx(t, "inbox")
	if out != halt+"\n"+`{"id":"s-2","kind":"set_max_cycles","n":5}`+"\n" {
		t.Fatalf("inbox: %q", out)
	}
	if _, again := bx(t, "inbox"); again != "" {
		t.Fatalf("inbox must consume messages: %q", again)
	}
}
