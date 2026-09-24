package terminal

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mjtechguy/blaxsmith/internal/terminal/terminaltest"
)

func newFakeGuest(t *testing.T) (*terminaltest.Fake, *Guest) {
	t.Helper()
	fake, conn := terminaltest.Start(t)
	guest, err := NewRouterConn(conn).Guest("blaxsmith-org", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	return fake, guest
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// serveSession runs Session behind a real WebSocket and returns a client.
func serveSession(t *testing.T, guest *Guest, control *atomic.Bool, wake chan struct{}) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s := &Session{Guest: guest, Wake: wake, Recheck: time.Hour, Check: func(context.Context) (Access, error) {
			state := State{Type: "state", Control: "agent", Stage: "implement", AttemptStatus: "running"}
			if control.Load() {
				holder := "principal"
				state.Control, state.Holder = "human", &holder
			}
			return Access{Control: control.Load(), State: state}, nil
		}}
		s.Serve(r.Context(), c)
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func readText(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for {
		kind, data, err := c.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.MessageText {
			var v map[string]any
			if err := json.Unmarshal(data, &v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
}

func TestViewerInputAndResizeDropped(t *testing.T) {
	fake, guest := newFakeGuest(t)
	var control atomic.Bool
	c := serveSession(t, guest, &control, nil)
	if state := readText(t, c); state["type"] != "state" || state["control"] != "agent" || state["holder"] != nil {
		t.Fatalf("initial state: %v", state)
	}
	ctx := t.Context()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("rm -rf /\r")); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "attach", func() bool { return len(fake.Commands("script")) == 1 })
	time.Sleep(200 * time.Millisecond)
	attach := fake.Snapshot()[0]
	if !slices.Equal(attach.Argv, []string{"script", "-qfc", "tmux -S /tmp/blaxsmith/tmux.sock attach -t agent -r", "/dev/null"}) {
		t.Fatalf("viewer attach: %q", attach.Argv)
	}
	if len(attach.Stdin) != 0 || len(fake.Commands("tmux")) != 0 {
		t.Fatalf("viewer input reached guest: %q, %v", attach.Stdin, fake.Commands("tmux"))
	}
	if fake.Snapshot()[0].Target != "blaxsmith-org/attempt-1" {
		t.Fatalf("routing metadata: %q", fake.Snapshot()[0].Target)
	}
	c.Close(websocket.StatusNormalClosure, "")
	eventually(t, "attach client killed on close", func() bool { return fake.Snapshot()[0].Killed })
}

func TestControllerInputResizeAndHandoverReattach(t *testing.T) {
	fake, guest := newFakeGuest(t)
	var control atomic.Bool
	wake := make(chan struct{}, 1)
	c := serveSession(t, guest, &control, wake)
	readText(t, c)
	eventually(t, "view attach", func() bool { return len(fake.Commands("script")) == 1 })

	control.Store(true) // takeover recorded; hub wakes the socket
	wake <- struct{}{}
	if state := readText(t, c); state["control"] != "human" || state["holder"] != "principal" {
		t.Fatalf("takeover state: %v", state)
	}
	eventually(t, "control attach", func() bool { return len(fake.Commands("script")) == 2 })
	procs := fake.Snapshot()
	if !procs[0].Killed || strings.HasSuffix(procs[1].Argv[2], "-r") {
		t.Fatalf("view client not replaced by control client: %+v", procs)
	}
	ctx := t.Context()
	if err := c.Write(ctx, websocket.MessageBinary, []byte("hello\r")); err != nil {
		t.Fatal(err)
	}
	if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":120,"rows":40}`)); err != nil {
		t.Fatal(err)
	}
	eventually(t, "controller input", func() bool { return string(fake.Snapshot()[1].Stdin) == "hello\r" })
	eventually(t, "controller resize", func() bool {
		got := fake.Commands("tmux")
		return len(got) == 1 && slices.Equal(got[0].Argv, []string{"tmux", "-S", TmuxSocket, "resize-window", "-t", "agent", "-x", "120", "-y", "40"})
	})

	control.Store(false) // handback demotes the socket back to read-only
	wake <- struct{}{}
	if state := readText(t, c); state["control"] != "agent" {
		t.Fatalf("handback state: %v", state)
	}
	eventually(t, "view reattach", func() bool { return len(fake.Commands("script")) == 3 })
	if argv := fake.Commands("script")[2].Argv; !strings.HasSuffix(argv[2], " -r") {
		t.Fatalf("demoted attach is writable: %q", argv)
	}
}

func TestAttachExitSendsExitFrame(t *testing.T) {
	fake, guest := newFakeGuest(t)
	var control atomic.Bool
	c := serveSession(t, guest, &control, nil)
	readText(t, c)
	eventually(t, "attach", func() bool { return len(fake.Commands("script")) == 1 })
	fake.Exit("1", 0)
	if frame := readText(t, c); frame["type"] != "exit" || frame["code"] != float64(0) {
		t.Fatalf("exit frame: %v", frame)
	}
	_, _, err := c.Read(t.Context())
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close after exit: %v", err)
	}
}

func TestTakeOverSequence(t *testing.T) {
	fake, guest := newFakeGuest(t)
	fake.Stdout = func(argv []string) string {
		switch {
		case argv[0] == ToolWorker:
			return `["claude","--bare","--resume","sess-1"]`
		case slices.Contains(argv, "display-message"):
			return "1\n"
		}
		return ""
	}
	if err := guest.TakeOver(t.Context()); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range fake.Snapshot() {
		got = append(got, strings.Join(p.Argv, " "))
	}
	want := []string{
		ToolWorker + " resume-argv",
		"tmux -S " + TmuxSocket + " send-keys -t agent C-c",
		"tmux -S " + TmuxSocket + " display-message -p -t agent #{pane_dead}",
		"tmux -S " + TmuxSocket + " respawn-pane -k -t agent -- " + ToolWorker + " pane --interactive -- claude --bare --resume sess-1",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("takeover commands:\n%s", strings.Join(got, "\n"))
	}
	before := len(fake.Snapshot())
	started := time.Now()
	if err := guest.HandBack(t.Context()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) < handBackPause {
		t.Fatalf("handback did not pause between /exit and Enter")
	}
	got = nil
	for _, p := range fake.Snapshot()[before:] {
		got = append(got, strings.Join(p.Argv, " "))
	}
	want = []string{
		"tmux -S " + TmuxSocket + " send-keys -t agent -l /exit",
		"tmux -S " + TmuxSocket + " send-keys -t agent Enter",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("handback commands:\n%s", strings.Join(got, "\n"))
	}
}

func TestTakeOverWithoutResumeLeavesAgentRunning(t *testing.T) {
	fake, guest := newFakeGuest(t)
	fake.Stdout = func([]string) string { return "not json" }
	if err := guest.TakeOver(t.Context()); err == nil {
		t.Fatal("takeover without resume argv succeeded")
	}
	if len(fake.Commands("tmux")) != 0 {
		t.Fatalf("agent interrupted without a resume path: %v", fake.Commands("tmux"))
	}
}

func TestReadFileBounded(t *testing.T) {
	_, guest := newFakeGuest(t)
	data, err := guest.ReadFile(t.Context(), "/tmp/blaxsmith/result.json", 1024)
	if err != nil || string(data) != "file:/tmp/blaxsmith/result.json" {
		t.Fatalf("read: %q %v", data, err)
	}
	if _, err := guest.ReadFile(t.Context(), "/tmp/blaxsmith/result.json", 4); err == nil {
		t.Fatal("oversized file accepted")
	}
}

func TestHubOnlyNewestHolderSocketControls(t *testing.T) {
	hub := NewHub()
	first := hub.Join("attempt", "session-a")
	viewer := hub.Join("attempt", "session-b")
	if !first.Controls("session-a") || viewer.Controls("session-a") || first.Controls("") {
		t.Fatal("holder socket control")
	}
	select { // joining wakes existing sockets so they recheck
	case <-first.Wake():
	default:
		t.Fatal("join did not wake existing socket")
	}
	reconnect := hub.Join("attempt", "session-a")
	if first.Controls("session-a") || !reconnect.Controls("session-a") {
		t.Fatal("reconnect created a second controller")
	}
	reconnect.Leave()
	if !first.Controls("session-a") {
		t.Fatal("remaining holder socket lost control")
	}
	first.Leave()
	viewer.Leave()
	if len(hub.members) != 0 {
		t.Fatal("hub leaked members")
	}
}

func TestGuestTargetValidated(t *testing.T) {
	r := NewRouterConn(nil)
	for _, bad := range [][2]string{{"", "a"}, {"a", ""}, {"a/b", "c"}, {"a", "b\nc"}, {"A", "b"}} {
		if _, err := r.Guest(bad[0], bad[1]); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := NewRouter("atenet-router.ate-system.svc:80"); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRouter("http://router"); err == nil {
		t.Fatal("URL accepted as router address")
	}
}
