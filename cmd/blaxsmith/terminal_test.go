package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/terminal/terminaltest"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestTerminalRejectsForeignOrigin(t *testing.T) {
	const origin = "https://blaxsmith.example"
	// Origin is checked before any session or database access.
	handler := newTerminalHandler(nil, origin, nil, nil, terminal.NewHub())
	for name, values := range map[string][]string{
		"missing":   nil,
		"foreign":   {"https://evil.example"},
		"scheme":    {"http://blaxsmith.example"},
		"subdomain": {"https://a.blaxsmith.example"},
		"duplicate": {origin, "https://evil.example"},
	} {
		req := httptest.NewRequest(http.MethodGet, origin+"/api/terminal/attempts/x", nil)
		req.Header.Set("Connection", "Upgrade")
		req.Header.Set("Upgrade", "websocket")
		for _, value := range values {
			req.Header.Add("Origin", value)
		}
		got := httptest.NewRecorder()
		handler.ServeHTTP(got, req)
		if got.Code != http.StatusForbidden {
			t.Fatalf("%s origin: %d", name, got.Code)
		}
	}
}

func TestTerminalAccessOnlyHolderSocketControls(t *testing.T) {
	hub := terminal.NewHub()
	holder := identity.Caller{PrincipalID: "p1", SessionID: "s1", Role: "member"}
	older, newer := hub.Join("a", "s1"), hub.Join("a", "s1")
	held := workflow.AttemptTerminal{State: "running", Stage: "implement", HolderPrincipalID: "p1", HolderSessionID: "s1"}
	if terminalAccess(held, holder, older).Control || !terminalAccess(held, holder, newer).Control {
		t.Fatal("reconnect must move control to the newest holder socket only")
	}
	demoted := holder
	demoted.Role = "viewer"
	if terminalAccess(held, demoted, newer).Control {
		t.Fatal("viewer role kept control")
	}
	done := held
	done.State = "succeeded"
	if access := terminalAccess(done, holder, newer); access.Control || access.State.Control != "agent" || access.State.Holder != nil {
		t.Fatalf("finished attempt still human-controlled: %+v", access)
	}
	if access := terminalAccess(held, identity.Caller{PrincipalID: "p2", SessionID: "s2", Role: "owner"}, hub.Join("a", "s2")); access.Control ||
		access.State.Control != "human" || *access.State.Holder != "p1" {
		t.Fatalf("viewer socket state: %+v", access)
	}
}

func TestTerminalTakeoverPostgres(t *testing.T) {
	pool := terminalTestPool(t)
	ctx := t.Context()
	password := []byte("correct horse battery staple")
	owner, err := identity.BootstrapOwner(ctx, pool, "alice", "engineering", "Engineering", password)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := identity.HashPassword(password)
	if err != nil {
		t.Fatal(err)
	}
	var bobID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_principals (id,username,password_hash)
		VALUES (gen_random_uuid(),'bob',$1) RETURNING id`, hash).Scan(&bobID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_memberships (organization_id,principal_id,role)
		VALUES ($1,$2,'member')`, owner.OrganizationID, bobID); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	attempt := runningTerminalAttempt(t, pool, store, owner.OrganizationID)

	fake, conn := terminaltest.Start(t)
	fake.Stdout = func(argv []string) string {
		switch {
		case argv[0] == terminal.ToolWorker:
			return `["claude","--bare","--resume","sess-1"]`
		case strings.Contains(strings.Join(argv, " "), "pane_dead"):
			return "1\n"
		}
		return ""
	}
	router := terminal.NewRouterConn(conn)
	hub := terminal.NewHub()
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	manager, err := identity.NewSessionManager(pool, origin, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	guard, err := identity.NewBrowserGuard(manager, origin)
	if err != nil {
		t.Fatal(err)
	}
	handler := newTerminalHandler(guard, origin, store, router, hub)
	handler.recheck = time.Hour // only hub wakes drive rechecks in this test
	mux := http.NewServeMux()
	mux.Handle("/api/terminal/attempts/{attemptID}", guard.Wrap(handler))
	server.Config.Handler = mux
	server.StartTLS()
	t.Cleanup(server.Close)
	service := &workflowService{guard: guard, store: store, guests: router, terminals: hub}

	login := func(user string) http.Header {
		tokens, err := manager.LoginLocal(ctx, "engineering", user, password, netip.MustParseAddr("127.0.0.1"))
		if err != nil {
			t.Fatal(err)
		}
		raw := make([]byte, 32)
		_, _ = rand.Read(raw)
		csrf := base64.RawURLEncoding.EncodeToString(raw)
		header := http.Header{}
		header.Set("Origin", origin)
		header.Set("Cookie", "__Host-blaxsmith_access="+tokens.Access+"; __Host-blaxsmith_csrf="+csrf)
		header.Set("X-Blaxsmith-CSRF", csrf)
		return header
	}
	alice, bob := login("alice"), login("bob")
	socketURL := "wss://" + strings.TrimPrefix(origin, "https://") + "/api/terminal/attempts/" + attempt.ID
	dial := func(header http.Header) *websocket.Conn {
		t.Helper()
		dialCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		c, _, err := websocket.Dial(dialCtx, socketURL, &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: header})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.CloseNow() })
		return c
	}
	frame := func(c *websocket.Conn) map[string]any {
		t.Helper()
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for {
			kind, data, err := c.Read(readCtx)
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
	scripts := func(n int) []terminaltest.Proc {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			got := fake.Commands("script")
			if len(got) >= n {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiting for %d attaches, have %d", n, len(got))
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	readOnly := func(p terminaltest.Proc) bool { return strings.HasSuffix(p.Argv[2], " -r") }
	rpc := func(header http.Header, msg any) error {
		switch m := msg.(type) {
		case *api.TakeOverAttemptRequest:
			req := connect.NewRequest(m)
			for k, v := range header {
				req.Header()[k] = v
			}
			_, err := service.TakeOverAttempt(ctx, req)
			return err
		case *api.HandBackAttemptRequest:
			req := connect.NewRequest(m)
			for k, v := range header {
				req.Header()[k] = v
			}
			_, err := service.HandBackAttempt(ctx, req)
			return err
		}
		return errors.New("unknown rpc")
	}
	canTakeOver := func(header http.Header) bool {
		req := connect.NewRequest(&api.GetAttemptControlRequest{AttemptId: attempt.ID})
		for k, v := range header {
			req.Header()[k] = v
		}
		resp, err := service.GetAttemptControl(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Msg.CanTakeOver
	}
	// Takeover shows the model key: the owner may, a member without a
	// personal model connection may not.
	if !canTakeOver(alice) || canTakeOver(bob) {
		t.Fatalf("can_take_over: owner %v, member %v", canTakeOver(alice), canTakeOver(bob))
	}

	// Cookie without a matching Origin is rejected before upgrade.
	foreign := alice.Clone()
	foreign.Set("Origin", "https://evil.example")
	if _, resp, err := websocket.Dial(ctx, socketURL, &websocket.DialOptions{HTTPClient: server.Client(), HTTPHeader: foreign}); err == nil ||
		resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin socket: %v", err)
	}

	aliceSocket := dial(alice)
	if state := frame(aliceSocket); state["type"] != "state" || state["control"] != "agent" || state["stage"] != "implement" ||
		state["attemptStatus"] != "running" {
		t.Fatalf("initial state: %v", state)
	}
	if got := scripts(1); !readOnly(got[0]) || got[0].Target != "blaxsmith-org/attempt-1" {
		t.Fatalf("initial attach: %+v", got[0])
	}
	bobSocket := dial(bob)
	frame(bobSocket)
	scripts(2)

	if err := rpc(alice, &api.TakeOverAttemptRequest{AttemptId: attempt.ID}); err != nil {
		t.Fatal(err)
	}
	if got := fake.Commands("tmux -S " + terminal.TmuxSocket + " respawn-pane"); len(got) != 1 {
		t.Fatal("guest takeover did not respawn the pane")
	}
	if state := frame(aliceSocket); state["control"] != "human" || state["holder"] != owner.PrincipalID {
		t.Fatalf("holder state: %v", state)
	}
	if state := frame(bobSocket); state["control"] != "human" {
		t.Fatalf("viewer state: %v", state)
	}
	if got := scripts(3); readOnly(got[2]) {
		t.Fatalf("holder did not reattach writable: %+v", got)
	}
	if err := rpc(bob, &api.TakeOverAttemptRequest{AttemptId: attempt.ID}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("member takeover without the model connection: %v", err)
	}
	if err := rpc(bob, &api.HandBackAttemptRequest{AttemptId: attempt.ID}); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-holder handback: %v", err)
	}
	// Bob's typing never reaches the guest.
	if err := bobSocket.Write(ctx, websocket.MessageBinary, []byte("bob\r")); err != nil {
		t.Fatal(err)
	}

	// Alice reconnects (e.g. a second tab): the new socket controls, the old one is demoted.
	aliceAgain := dial(alice)
	frame(aliceAgain)
	if state := frame(aliceSocket); state["control"] != "human" {
		t.Fatalf("demoted socket state: %v", state)
	}
	got := scripts(5)
	writable := 0
	for _, p := range got {
		if !readOnly(p) && !p.Killed {
			writable++
		}
	}
	if writable != 1 {
		t.Fatalf("live writable attaches: %d (%+v)", writable, got)
	}
	for _, p := range fake.Snapshot() {
		if strings.Contains(string(p.Stdin), "bob") {
			t.Fatal("viewer input reached the guest")
		}
	}

	if err := rpc(alice, &api.HandBackAttemptRequest{AttemptId: attempt.ID}); err != nil {
		t.Fatal(err)
	}
	if len(fake.Commands("tmux -S "+terminal.TmuxSocket+" send-keys -t agent -l /exit")) != 1 {
		t.Fatal("handback did not exit the native TUI")
	}
	if state := frame(aliceAgain); state["control"] != "agent" || state["holder"] != nil {
		t.Fatalf("handback state: %v", state)
	}

	// Session revocation closes the live socket on the next recheck.
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp() WHERE principal_id=$1`, bobID); err != nil {
		t.Fatal(err)
	}
	hub.Notify(attempt.ID)
	for {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		kind, data, err := bobSocket.Read(readCtx)
		cancel()
		if err != nil {
			t.Fatalf("revoked socket closed without error frame: %v", err)
		}
		if kind == websocket.MessageText && strings.Contains(string(data), `"type":"error"`) {
			break
		}
	}
}

func runningTerminalAttempt(t *testing.T, pool *pgxpool.Pool, store *workflow.Store, org string) workflow.Attempt {
	t.Helper()
	ctx := t.Context()
	project, err := store.CreateProject(ctx, org, "terminal-project", "Terminal project")
	if err != nil {
		t.Fatal(err)
	}
	input := workflow.RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "terminal-run",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "implement", input.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BindRuntime(ctx, attempt, workflow.RuntimeBinding{AXAtespace: "blaxsmith-org", AXTask: "attempt-1",
		ActorUID: "uid", TemplateUID: "tpl", Image: "runner@sha256:" + strings.Repeat("d", 64),
		WorkerPool: "pool", CommandSHA256: strings.Repeat("e", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	return attempt
}

func terminalTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var random [8]byte
	_, _ = rand.Read(random[:])
	schema := "blaxsmith_terminal_" + hex.EncodeToString(random[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	u.RawQuery = query.Encode()
	pool, err := pgxpool.New(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
