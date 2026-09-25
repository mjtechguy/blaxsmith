package interact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/guild"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// fakeGuest scripts `bx watch` sessions and records every other command.
type fakeGuest struct {
	mu       sync.Mutex
	sessions [][]string      // lines per watch session, consumed in order
	hangups  []chan struct{} // closing one ends that watch session
	cursors  []string
	calls    [][]string
	exit     int // exit code for bx answer / bx steer
}

func (g *fakeGuest) Start(ctx context.Context, _ workflow.Attempt, argv []string, _ bool) (*Process, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if argv[1] != "watch" {
		g.calls = append(g.calls, argv)
		code := g.exit
		return &Process{Stdout: strings.NewReader(""), Wait: func() (int, error) { return code, nil }}, nil
	}
	g.cursors = append(g.cursors, argv[3])
	if len(g.sessions) == 0 {
		return nil, errors.New("guest unavailable")
	}
	lines, hangup := g.sessions[0], g.hangups[0]
	g.sessions, g.hangups = g.sessions[1:], g.hangups[1:]
	r, w := io.Pipe()
	go func() {
		for _, line := range lines {
			if _, err := io.WriteString(w, line+"\n"); err != nil {
				return
			}
		}
		select {
		case <-hangup:
		case <-ctx.Done():
		}
		w.Close()
	}()
	return &Process{Stdout: r, Wait: func() (int, error) { return 0, nil }}, nil
}

func (g *fakeGuest) snapshot() ([]string, [][]string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.cursors), slices.Clone(g.calls)
}

func TestWatcherPersistDedupeRedeliverPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := tenant.System(t.Context())
	question := `{"id":"q1","kind":"question","title":"Pick a color","body_md":"## evidence","options":[{"id":"a","label":"Red","recommended":true},{"id":"b","label":"Blue"}],"allow_free_text":true,"blocking":true}`
	long := strings.Repeat("x", 20000)
	session1 := []string{
		`{"seq":1,"interaction":` + question + `}`,
		`{"seq":2,"event":{"type":"cycle","cycle":1,"max_cycles":3,"text":"` + long + `"}}`,
		`{"seq":3,"interaction":{"id":"bad id","kind":"question","title":"x","allow_free_text":true}}`,
	}
	// The second session replays everything (as if the guest lost the cursor).
	session2 := append(slices.Clone(session1), `{"seq":4,"event":{"type":"phase","name":"verify"}}`)
	hang1, hang2 := make(chan struct{}), make(chan struct{})
	guest := &fakeGuest{sessions: [][]string{session1, session2}, hangups: []chan struct{}{hang1, hang2}, exit: 1}
	watcher := &Watcher{Store: f.store, Guest: guest, Poll: time.Hour} // only wake-ups and reconnects deliver
	watchCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); watcher.Run(watchCtx, f.attempt) }()
	defer func() { stop(); <-done }()

	eventually(t, func() bool { c, _ := f.store.Cursor(ctx, f.attempt); return c == 3 })
	records, err := f.store.ListInteractions(ctx, f.org, f.attempt.RunID)
	if err != nil || len(records) != 1 || records[0].Interaction.ID != "q1" || records[0].State != "open" || records[0].Stage != "implement" {
		t.Fatalf("persisted interactions: %+v, %v", records, err)
	}
	// Answer while the guest refuses delivery: persisted, not delivered.
	answered, err := f.store.Answer(ctx, f.owner, records[0].ID, []string{"a"}, " and stripes ")
	if err != nil || answered.State != "answered" || answered.Answer.Text != "and stripes" {
		t.Fatalf("answer: %+v, %v", answered, err)
	}
	if _, err := f.store.Answer(ctx, f.owner, records[0].ID, []string{"b"}, ""); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("second answer: %v", err)
	}
	steerID, err := f.store.Steer(ctx, f.owner, f.attempt.ID, "set_max_cycles", "", "", 5)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, calls := guest.snapshot(); return len(calls) >= 1 })
	if n := f.count(`SELECT count(*) FROM workflow_interactions WHERE delivered_at IS NOT NULL`); n != 0 {
		t.Fatalf("failed delivery marked delivered: %d", n)
	}
	// Reconnect: guest now accepts; undelivered answer and steer are redelivered.
	guest.mu.Lock()
	guest.exit = 0
	guest.mu.Unlock()
	close(hang1)
	eventually(t, func() bool { c, _ := f.store.Cursor(ctx, f.attempt); return c == 4 })
	eventually(t, func() bool {
		return f.count(`SELECT count(*) FROM workflow_interactions WHERE delivered_at IS NOT NULL`) == 1 &&
			f.count(`SELECT count(*) FROM workflow_attempt_steers WHERE delivered_at IS NOT NULL`) == 1
	})
	cursors, calls := guest.snapshot()
	if len(cursors) < 2 || cursors[0] != "0" || cursors[1] != "3" {
		t.Fatalf("watch cursors: %v", cursors)
	}
	var sawAnswer, sawSteer bool
	for _, call := range calls {
		if call[1] == "answer" && call[2] == "q1" && call[3] == "--json" {
			var a Answer
			sawAnswer = json.Unmarshal([]byte(call[4]), &a) == nil && a.InteractionID == "q1" &&
				slices.Equal(a.OptionIDs, []string{"a"}) && a.AnsweredBy == f.owner.PrincipalID
		}
		var steer map[string]any
		if call[1] == "steer" && json.Unmarshal([]byte(call[3]), &steer) == nil && steer["id"] == steerID &&
			steer["kind"] == "set_max_cycles" && steer["n"] == float64(5) {
			sawSteer = true
		}
	}
	if !sawAnswer || !sawSteer {
		t.Fatalf("redelivery calls: %v", calls)
	}
	// Dedupe: the replayed seq 1-3 created nothing new.
	if n := f.count(`SELECT count(*) FROM workflow_interactions`); n != 1 {
		t.Fatalf("interactions after replay: %d", n)
	}
	kinds := f.eventKinds()
	if kinds["interaction.opened"] != 1 || kinds["attempt.progress"] != 2 || kinds["interaction.answered"] != 1 || kinds["attempt.control"] != 1 {
		t.Fatalf("event kinds: %v", kinds)
	}
	events, err := f.workflow.EventsAfter(ctx, f.org, f.attempt.RunID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var progress []string
	for _, e := range events {
		if e.Kind == "attempt.progress" {
			progress = append(progress, e.PayloadJSON)
		} else if e.PayloadJSON != "" {
			t.Fatalf("%s carried payload %q", e.Kind, e.PayloadJSON)
		}
	}
	var cycle map[string]any
	if len(progress) != 2 || len(progress[0]) > 8192 || json.Unmarshal([]byte(progress[0]), &cycle) != nil ||
		cycle["type"] != "cycle" || cycle["max_cycles"] != float64(3) || !strings.HasSuffix(cycle["text"].(string), "…") ||
		!strings.Contains(progress[1], `"verify"`) {
		t.Fatalf("progress payloads: %v", progress)
	}
	// Cursor replay for the browser stream: events after the first progress.
	after, err := f.workflow.EventsAfter(ctx, f.org, f.attempt.RunID, events[len(events)-2].ID, 100)
	if err != nil || len(after) != 1 || after[0].ID != events[len(events)-1].ID {
		t.Fatalf("cursor replay: %+v, %v", after, err)
	}
	// Stopping the attempt closes open interactions.
	stop()
	<-done
	close(hang2)
}

func TestAnswerAndSteerAuthorizationPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := tenant.System(t.Context())
	line := func(seq int, ix string) {
		if err := f.store.Persist(ctx, f.attempt, []byte(`{"seq":`+string(rune('0'+seq))+`,"interaction":`+ix+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	line(1, `{"id":"single","kind":"approval","title":"Go?","options":[{"id":"go","label":"Go"},{"id":"stop","label":"Stop"}]}`)
	line(2, `{"id":"multi","kind":"question","title":"Which?","multi_select":true,"options":[{"id":"x","label":"X"},{"id":"y","label":"Y"}]}`)
	records, err := f.store.ListInteractions(ctx, f.org, f.attempt.RunID)
	if err != nil || len(records) != 2 {
		t.Fatalf("records: %+v %v", records, err)
	}
	single, multi := records[0].ID, records[1].ID
	viewer := caller(t, f.pool, f.org, "viewer", "viewer")
	if _, err := f.store.Answer(ctx, viewer, single, []string{"go"}, ""); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer answered: %v", err)
	}
	if _, err := f.store.Steer(ctx, viewer, f.attempt.ID, "pause", "", "", 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer steered: %v", err)
	}
	outsider := caller(t, f.pool, organization(t, f.pool, "other"), "owner", "outsider")
	if _, err := f.store.Answer(ctx, outsider, single, []string{"go"}, ""); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("outsider answered: %v", err)
	}
	if _, err := f.store.Steer(ctx, outsider, f.attempt.ID, "pause", "", "", 0); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("outsider steered: %v", err)
	}
	revoked := caller(t, f.pool, f.org, "member", "revoked")
	if _, err := f.pool.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, revoked.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Answer(ctx, revoked, single, []string{"go"}, ""); !errors.Is(err, workflow.ErrFenced) {
		t.Fatalf("revoked session answered: %v", err)
	}
	member := caller(t, f.pool, f.org, "member", "member")
	for name, bad := range map[string]struct {
		id   string
		opts []string
		text string
	}{
		"unknown option":   {single, []string{"maybe"}, ""},
		"free text denied": {single, []string{"go"}, "because"},
		"two options":      {single, []string{"go", "stop"}, ""},
		"empty":            {single, nil, ""},
		"duplicate":        {multi, []string{"x", "x"}, ""},
		"bad id":           {"not-a-uuid", []string{"go"}, ""},
	} {
		if _, err := f.store.Answer(ctx, member, bad.id, bad.opts, bad.text); !errors.Is(err, workflow.ErrInvalid) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	if r, err := f.store.Answer(ctx, member, multi, []string{"y", "x"}, ""); err != nil || r.State != "answered" {
		t.Fatalf("multi answer: %+v %v", r, err)
	}
	var audit int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE action='workflow.interaction.answered'
		AND actor_id=$1 AND subject_id=$2`, member.PrincipalID, multi).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("answer audit: %d %v", audit, err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE workflow_interactions SET answer='{"x":1}' WHERE id=$1`, multi); err == nil {
		t.Fatal("answered interaction rewritten")
	}
	for name, bad := range map[string][3]string{
		"unknown kind":        {"reboot", "", ""},
		"instruction no text": {"instruction", "", ""},
		"halt no reason":      {"halt", "", ""},
		"pause with text":     {"pause", "x", ""},
		"long text":           {"instruction", strings.Repeat("a", MaxAnswerText+1), ""},
	} {
		if _, err := f.store.Steer(ctx, member, f.attempt.ID, bad[0], bad[1], bad[2], 0); !errors.Is(err, workflow.ErrInvalid) {
			t.Fatalf("%s steer accepted: %v", name, err)
		}
	}
	if _, err := f.store.Steer(ctx, member, f.attempt.ID, "set_max_cycles", "", "", 101); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatalf("cap over bound accepted: %v", err)
	}
	if _, err := f.store.Steer(ctx, member, f.attempt.ID, "halt", "", "tests are flaky", 0); err != nil {
		t.Fatal(err)
	}
	// Stopped attempt: answers and steers are refused; CancelOpen closes the rest.
	if _, err := f.pool.Exec(ctx, `UPDATE workflow_attempts SET state='stopped' WHERE id=$1`, f.attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Answer(ctx, member, single, []string{"go"}, ""); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("answer after stop: %v", err)
	}
	if _, err := f.store.Steer(ctx, member, f.attempt.ID, "pause", "", "", 0); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("steer after stop: %v", err)
	}
	if err := f.store.CancelOpen(ctx, f.attempt); err != nil {
		t.Fatal(err)
	}
	records, _ = f.store.ListInteractions(ctx, f.org, f.attempt.RunID)
	if records[0].State != "cancelled" || records[1].State != "answered" {
		t.Fatalf("states after stop: %s %s", records[0].State, records[1].State)
	}
}

func TestEscalationAndTranscriptPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := tenant.System(t.Context())
	ix := Interaction{ID: "loop-cap-verify-3", Title: "Verify loop hit its cap", BodyMD: "3 cycles failed",
		Options: []Option{{ID: "raise", Label: "Raise cap"}, {ID: "halt", Label: "Halt"}}}
	id, err := f.store.Raise(ctx, f.org, f.attempt.RunID, "implement", ix)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := f.store.Raise(ctx, f.org, f.attempt.RunID, "implement", ix); err != nil || again != id {
		t.Fatalf("raise replay: %s %v", again, err)
	}
	if _, err := f.store.Raise(ctx, f.org, f.attempt.RunID, "missing", ix); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("unknown stage: %v", err)
	}
	var mu sync.Mutex
	var got []string
	fail := true
	f.store.OnEscalationAnswer(func(_ context.Context, orgID, runID, stage, interactionID string, a Answer) error {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			fail = false
			return errors.New("engine busy")
		}
		got = append(got, stage+"/"+interactionID+"/"+strings.Join(a.OptionIDs, ","))
		return nil
	})
	if r, err := f.store.Answer(ctx, f.owner, id, []string{"raise"}, ""); err != nil || r.AttemptID != "" || r.Kind != "escalation" {
		t.Fatalf("escalation answer: %+v %v", r, err)
	}
	// The first delivery fails; the coordinator's redelivery tick retries it.
	eventually(t, func() bool {
		f.store.DeliverEscalations(ctx)
		mu.Lock()
		defer mu.Unlock()
		return len(got) == 1
	})
	mu.Lock()
	if len(got) != 1 || got[0] != "implement/"+id+"/raise" {
		t.Fatalf("escalation callback: %v", got)
	}
	mu.Unlock()
	f.store.DeliverEscalations(ctx)
	if len(got) != 1 {
		t.Fatalf("delivered twice: %v", got)
	}

	// Transcript: three answered rounds plus a finalize round and an approval.
	rounds := []string{
		`{"id":"r1","kind":"interview_round","title":"What must stay frozen?","options":[{"id":"commit","label":"Freeze recipe inputs at an exact Git commit."},{"id":"done","label":"Done"}],"allow_free_text":true,"interview":{"round":1,"finalize_option":"done","tags":["ARCH_INVARIANT"]}}`,
		`{"id":"r2","kind":"interview_round","title":"Where does review sit?","allow_free_text":true,"interview":{"round":2,"finalize_option":"done"},"options":[{"id":"done","label":"Done"}]}`,
		`{"id":"r3","kind":"question","title":"Does this execute workers?","allow_free_text":true,"interview":{"round":3,"tags":["IMPLICIT_FACT:RUNTIME"]}}`,
		`{"id":"r4","kind":"interview_round","title":"Anything else?","options":[{"id":"done","label":"Done"}],"interview":{"round":4,"finalize_option":"done"}}`,
		`{"id":"g1","kind":"approval","title":"Proceed?","options":[{"id":"go","label":"Go"}]}`,
	}
	for i, r := range rounds {
		if err := f.store.Persist(ctx, f.attempt, []byte(`{"seq":`+string(rune('1'+i))+`,"interaction":`+r+`}`)); err != nil {
			t.Fatal(err)
		}
	}
	records, _ := f.store.ListInteractions(ctx, f.org, f.attempt.RunID)
	answers := map[string][2]any{
		"r1": {[]string{"commit"}, ""},
		"r2": {[]string{}, "Keep human review as the final stage."},
		"r3": {[]string{}, "This example only compiles a recipe; it does not execute workers. No application state transitions or external service contracts are needed for this example."},
		"r4": {[]string{"done"}, ""},
		"g1": {[]string{"go"}, ""},
	}
	for _, r := range records {
		if a, ok := answers[r.Interaction.ID]; ok {
			if _, err := f.store.Answer(ctx, f.owner, r.ID, a[0].([]string), a[1].(string)); err != nil {
				t.Fatalf("answer %s: %v", r.Interaction.ID, err)
			}
		}
	}
	transcript, err := f.store.StageTranscript(ctx, f.org, f.attempt.RunID, "implement")
	if err != nil {
		t.Fatal(err)
	}
	text := string(transcript)
	for _, want := range []string{"## Q-001\n**Question:** What must stay frozen?\n**Options presented:** Freeze recipe inputs at an exact Git commit. | Done\n",
		"## A-001 [ARCH_INVARIANT]\nFreeze recipe inputs at an exact Git commit. [from Q-001]\n",
		"## A-002\nKeep human review as the final stage. [from Q-002]\n",
		"## A-003 [IMPLICIT_FACT:RUNTIME]\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("transcript missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Q-004") || strings.Contains(text, "Proceed?") {
		t.Fatalf("finalize round or approval rendered:\n%s", text)
	}
	spec, err := os.ReadFile("../../examples/guild/spec.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guild.Validate(ctx, spec, transcript); err != nil {
		t.Fatalf("guild.Validate rejected rendered transcript: %v\n%s", err, text)
	}
}

func TestProgressPayloadTruncates(t *testing.T) {
	big := map[string]any{"type": "finding", "text": strings.Repeat("é", 9000)}
	for i := range 40 {
		big["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = strings.Repeat("z", 400)
	}
	raw, _ := json.Marshal(big)
	got, err := progressPayload(raw)
	if err != nil || len(got) > 8192 || !strings.Contains(string(got), `"type":"finding"`) {
		t.Fatalf("payload %d bytes: %v", len(got), err)
	}
	items := make([]map[string]string, 400)
	for i := range items {
		items[i] = map[string]string{"text": strings.Repeat("step ", 20), "status": "pending"}
	}
	plan, _ := json.Marshal(map[string]any{"type": "plan.updated", "id": "plan", "items": items})
	if got, err := progressPayload(plan); err != nil || len(got) > 8192 || !strings.Contains(string(got), `"type":"plan.updated"`) {
		t.Fatalf("plan payload %d bytes: %v", len(got), err)
	}
	for _, kind := range []string{"tool.started", "tool.completed", "command", "file.changed", "assistant.message"} {
		if _, err := progressPayload([]byte(`{"type":"` + kind + `","id":"x"}`)); err != nil {
			t.Fatalf("rejected work-log type %s", kind)
		}
	}
	for _, bad := range []string{`[]`, `{"type":"shell"}`, `null`, `{"text":"x"}`} {
		if _, err := progressPayload([]byte(bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
}

type fixture struct {
	pool     *pgxpool.Pool
	store    *Store
	workflow *workflow.Store
	org      string
	owner    identity.Caller
	attempt  workflow.Attempt
}

func (f fixture) count(query string) int {
	var n int
	if err := f.pool.QueryRow(tenant.System(context.Background()), query).Scan(&n); err != nil {
		panic(err)
	}
	return n
}

func (f fixture) eventKinds() map[string]int {
	rows, err := f.pool.Query(tenant.System(context.Background()), `SELECT kind,count(*) FROM workflow_events GROUP BY kind`)
	if err != nil {
		panic(err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (struct {
		k string
		n int
	}, error) {
		var v struct {
			k string
			n int
		}
		return v, row.Scan(&v.k, &v.n)
	})
	if err != nil {
		panic(err)
	}
	kinds := map[string]int{}
	for _, v := range out {
		kinds[v.k] = v.n
	}
	return kinds
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	wf, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "ix")
	project, err := wf.CreateProject(ctx, org, "ix-project", "Interactions")
	if err != nil {
		t.Fatal(err)
	}
	run, err := wf.CreateRun(ctx, workflow.RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "ix",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := wf.AddTask(ctx, org, run.ID, "implement", strings.Repeat("b", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := wf.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := wf.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := wf.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	return fixture{pool: pool, store: store, workflow: wf, org: org, owner: caller(t, pool, org, "owner", "owner"), attempt: attempt}
}

func eventually(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	ctx := tenant.System(t.Context())
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	_, _ = rand.Read(suffix[:])
	schema := "blaxsmith_interact_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(tenant.System(context.Background()), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, tenant.Configure(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func organization(t *testing.T, pool *pgxpool.Pool, suffix string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(tenant.System(t.Context()), `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),$1,$2) RETURNING id`, "org-"+suffix, "Organization "+suffix).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func caller(t *testing.T, pool *pgxpool.Pool, orgID, role, username string) identity.Caller {
	t.Helper()
	var principal, session string
	if err := pool.QueryRow(tenant.System(t.Context()), `INSERT INTO identity_principals (id,username)
		VALUES (gen_random_uuid(),$1) RETURNING id`, username).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(tenant.System(t.Context()), `INSERT INTO identity_memberships (organization_id,principal_id,role)
		VALUES ($1,$2,$3)`, orgID, principal, role); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(tenant.System(t.Context()), `INSERT INTO identity_sessions
		(organization_id,id,principal_id,auth_method,mfa_level,expires_at)
		VALUES ($1,gen_random_uuid(),$2,'local','none',clock_timestamp()+interval '1 hour') RETURNING id`,
		orgID, principal).Scan(&session); err != nil {
		t.Fatal(err)
	}
	return identity.Caller{OrganizationID: orgID, PrincipalID: principal, SessionID: session,
		Role: role, AccessExpires: time.Now().Add(10 * time.Minute)}
}
