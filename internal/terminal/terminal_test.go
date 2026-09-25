package terminal

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/mjtechguy/blaxsmith/internal/terminal/terminaltest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	return serveAs(t, guest, control, wake, true)
}

// serveAs serves one socket; holder=false makes it a bystander that is hidden
// while control is human.
func serveAs(t *testing.T, guest *Guest, control *atomic.Bool, wake chan struct{}, holder bool) *websocket.Conn {
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
			human := control.Load()
			return Access{Control: human && holder, Hidden: human && !holder, State: state}, nil
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

func TestBystanderHiddenDuringTakeover(t *testing.T) {
	fake, guest := newFakeGuest(t)
	var control atomic.Bool
	wake := make(chan struct{}, 1)
	c := serveAs(t, guest, &control, wake, false)
	readText(t, c)
	eventually(t, "view attach", func() bool { return len(fake.Commands("script")) == 1 })

	control.Store(true) // someone else took over: the unredacted TUI must not reach this socket
	wake <- struct{}{}
	if state := readText(t, c); state["control"] != "human" {
		t.Fatalf("takeover state: %v", state)
	}
	eventually(t, "view detach", func() bool { return fake.Snapshot()[0].Killed })
	time.Sleep(50 * time.Millisecond)
	if n := len(fake.Commands("script")); n != 1 {
		t.Fatalf("hidden socket attached: %d attaches", n)
	}

	control.Store(false)
	wake <- struct{}{}
	if state := readText(t, c); state["control"] != "agent" {
		t.Fatalf("handback state: %v", state)
	}
	eventually(t, "view reattach", func() bool { return len(fake.Commands("script")) == 2 })
	if argv := fake.Commands("script")[1].Argv; !strings.HasSuffix(argv[2], " -r") {
		t.Fatalf("reattach is writable: %q", argv)
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
}

// routerTLS returns server credentials for localhost and a CA file trusting them.
func routerTLS(t *testing.T) (credentials.TransportCredentials, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "router-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return credentials.NewServerTLSFromCert(&tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}), caFile
}

func TestNewRouterRequiresTLSAndToken(t *testing.T) {
	_, caFile := routerTLS(t)
	dir := t.TempDir()
	token, blank, badCA := filepath.Join(dir, "token"), filepath.Join(dir, "blank"), filepath.Join(dir, "bad-ca")
	for path, data := range map[string]string{token: "tok\n", blank: " \n", badCA: "not a certificate"} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range [][3]string{
		{"http://router", caFile, token},
		{"router:443", "", token},
		{"router:443", caFile, ""},
		{"router:443", badCA, token},
		{"router:443", filepath.Join(dir, "missing"), token},
		{"router:443", caFile, filepath.Join(dir, "missing")},
		{"router:443", caFile, blank},
	} {
		if _, err := NewRouter(bad[0], bad[1], bad[2]); err == nil {
			t.Fatalf("NewRouter%q dialed without valid TLS and token", bad)
		}
	}
	r, err := NewRouter("atenet-router.ate-system.svc:443", caFile, token)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
}

// The kubelet rotates a projected token by writing a new timestamped directory
// and swapping the ..data symlink that token points through.
func TestRouterFollowsProjectedTokenRotation(t *testing.T) {
	serverCreds, caFile := routerTLS(t)
	fake, dialer := terminaltest.Serve(t, grpc.Creds(serverCreds))
	dir := t.TempDir()
	project := func(name, token string) {
		t.Helper()
		if err := os.Mkdir(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "token"), []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(name, filepath.Join(dir, "..data_tmp")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
	}
	project("..2026_09_24_01", "sa-one")
	token := filepath.Join(dir, "token")
	if err := os.Symlink(filepath.Join("..data", "token"), token); err != nil {
		t.Fatal(err)
	}
	r, err := NewRouter("localhost:443", caFile, token, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	guest, _ := r.Guest("blaxsmith-org", "attempt-1")
	for _, next := range []string{"..2026_09_24_02", ""} {
		if _, _, err := guest.Exec(context.Background(), []string{"true"}, 64); err != nil {
			t.Fatal(err)
		}
		if next != "" {
			project(next, "sa-two")
		}
	}
	procs := fake.Snapshot()
	if len(procs) != 2 || procs[0].Auth != "Bearer sa-one" || procs[1].Auth != "Bearer sa-two" {
		t.Fatalf("router calls = %+v", procs)
	}
}

func TestRouterSendsTokenPerRPCOverTLS(t *testing.T) {
	serverCreds, caFile := routerTLS(t)
	fake, dialer := terminaltest.Serve(t, grpc.Creds(serverCreds))
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewRouter("localhost:443", caFile, token, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	guest, err := r.Guest("blaxsmith-org", "attempt-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := guest.Exec(context.Background(), []string{"true"}, 64); err != nil {
		t.Fatal(err)
	}
	// A re-minted token file applies to the next RPC.
	if err := os.WriteFile(token, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := guest.Exec(context.Background(), []string{"true"}, 64); err != nil {
		t.Fatal(err)
	}
	// An unreadable token fails the RPC instead of calling without one.
	if err := os.WriteFile(token, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := guest.Exec(context.Background(), []string{"true"}, 64); err == nil {
		t.Fatal("RPC sent without a token")
	}
	procs := fake.Snapshot()
	if len(procs) != 2 || procs[0].Auth != "Bearer first" || procs[1].Auth != "Bearer second" ||
		procs[0].Target != "blaxsmith-org/attempt-1" {
		t.Fatalf("router calls = %+v", procs)
	}

	// A router certificate outside the pinned CA is refused.
	_, otherCA := routerTLS(t)
	if err := os.WriteFile(token, []byte("third"), 0o600); err != nil {
		t.Fatal(err)
	}
	untrusted, err := NewRouter("localhost:443", otherCA, token, dialer)
	if err != nil {
		t.Fatal(err)
	}
	defer untrusted.Close()
	guest, _ = untrusted.Guest("blaxsmith-org", "attempt-1")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := guest.Exec(ctx, []string{"true"}, 64); err == nil || len(fake.Snapshot()) != 2 {
		t.Fatal("untrusted router certificate accepted")
	}
}

// slowClient records every frame a viewer receives; it acks only on request.
type slowClient struct {
	c      *websocket.Conn
	mu     sync.Mutex
	sizes  []int
	text   []map[string]any
	closed error
}

func dialWindowed(t *testing.T, guest *Guest, window Window) *slowClient {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		s := &Session{Guest: guest, Recheck: time.Hour, Window: window, Check: func(context.Context) (Access, error) {
			return Access{State: State{Type: "state", Control: "agent", Stage: "implement", AttemptStatus: "running"}}, nil
		}}
		s.Serve(r.Context(), c)
	}))
	t.Cleanup(server.Close)
	c, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.CloseNow() })
	client := &slowClient{c: c}
	go func() {
		for {
			kind, data, err := c.Read(context.Background())
			client.mu.Lock()
			switch {
			case err != nil:
				client.closed = err
			case kind == websocket.MessageBinary:
				client.sizes = append(client.sizes, len(data))
			default:
				var v map[string]any
				_ = json.Unmarshal(data, &v)
				client.text = append(client.text, v)
			}
			client.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return client
}

func (s *slowClient) received() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.sizes {
		total += n
	}
	return len(s.sizes), total
}

func (s *slowClient) ack(t *testing.T, n int) {
	t.Helper()
	if err := s.c.Write(t.Context(), websocket.MessageText, fmt.Appendf(nil, `{"type":"ack","bytes":%d}`, n)); err != nil {
		t.Fatal(err)
	}
}

// settled waits for in-flight frames, then reports the frame count.
func (s *slowClient) settled() int {
	time.Sleep(150 * time.Millisecond)
	n, _ := s.received()
	return n
}

func TestOutputWindowWaitsForAcknowledgements(t *testing.T) {
	fake, guest := newFakeGuest(t)
	client := dialWindowed(t, guest, Window{Chunks: 4, Bytes: 64 << 10, Buffer: 1 << 20})
	eventually(t, "attach", func() bool { return len(fake.Commands("script")) == 1 })
	for range 11 {
		fake.Output("1", []byte(strings.Repeat("x", 1000)))
	}
	// 12 frames are ready ("screen\r\n" plus 11); only a window of 4 is sent.
	eventually(t, "first window", func() bool { n, _ := client.received(); return n == 4 })
	if n := client.settled(); n != 4 {
		t.Fatalf("sent %d frames without acknowledgement", n)
	}
	client.ack(t, len("screen\r\n")+1000) // two frames rendered: two more may go
	eventually(t, "window reopened", func() bool { n, _ := client.received(); return n == 6 })
	if n := client.settled(); n != 6 {
		t.Fatalf("ack of two frames released %d", n-4)
	}
	client.ack(t, 500) // a partial frame retires nothing
	if n := client.settled(); n != 6 {
		t.Fatalf("partial ack released a frame: %d", n)
	}
	client.ack(t, 1<<30) // an ack larger than the wire only retires what was sent
	eventually(t, "next window", func() bool { n, _ := client.received(); return n == 10 })
	if n := client.settled(); n != 10 {
		t.Fatalf("oversized ack pre-paid output: %d frames", n)
	}
	client.ack(t, 4000)
	eventually(t, "all output", func() bool { n, total := client.received(); return n == 12 && total == len("screen\r\n")+11000 })
}

func TestSlowViewerIsDroppedWithErrorFrame(t *testing.T) {
	fake, guest := newFakeGuest(t)
	client := dialWindowed(t, guest, Window{Chunks: 2, Bytes: 8 << 10, Buffer: 16 << 10})
	eventually(t, "attach", func() bool { return len(fake.Commands("script")) == 1 })
	for range 10 { // the viewer never acks: 40 KiB overflows the 16 KiB buffer
		fake.Output("1", []byte(strings.Repeat("y", 4<<10)))
	}
	eventually(t, "viewer dropped", func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.closed != nil
	})
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.sizes) > 2 {
		t.Fatalf("window exceeded without acknowledgement: %v", client.sizes)
	}
	last := client.text[len(client.text)-1]
	if last["type"] != "error" || last["message"] != ErrSlowViewer {
		t.Fatalf("error frame: %v", client.text)
	}
	if websocket.CloseStatus(client.closed) != websocket.StatusPolicyViolation {
		t.Fatalf("close: %v", client.closed)
	}
	eventually(t, "attach client killed", func() bool { return fake.Snapshot()[0].Killed })
}
