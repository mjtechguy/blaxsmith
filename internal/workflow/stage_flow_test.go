package workflow

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestParseAttemptResult(t *testing.T) {
	good := `{"schema":"blaxsmith.attempt-result/v1alpha1","summary":"ok","revision":"` + strings.Repeat("a", 40) + `","verdict":"fail"}`
	if r, err := ParseAttemptResult([]byte(good)); err != nil || r.Verdict != "fail" || r.Summary != "ok" {
		t.Fatalf("valid result: %+v, %v", r, err)
	}
	for _, bad := range []string{
		`{"schema":"other","summary":"","revision":"","verdict":""}`,
		`{"schema":"blaxsmith.attempt-result/v1alpha1","summary":"","revision":"HEAD","verdict":""}`,
		`{"schema":"blaxsmith.attempt-result/v1alpha1","summary":"","revision":"","verdict":"maybe"}`,
		`{"schema":"blaxsmith.attempt-result/v1alpha1","summary":"","revision":"","verdict":"","extra":1}`,
		good + good,
	} {
		if _, err := ParseAttemptResult([]byte(bad)); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted %s", bad)
		}
	}
	long := `{"schema":"blaxsmith.attempt-result/v1alpha1","summary":"` + strings.Repeat("é", 9000) + `","revision":"","verdict":""}`
	if r, err := ParseAttemptResult([]byte(long)); err != nil || len(r.Summary) > maxSummaryBytes || !strings.HasSuffix(r.Summary, "é") {
		t.Fatalf("summary not bounded on a rune boundary: %d, %v", len(r.Summary), err)
	}
}

type recordedSink struct{ raised []Escalation }

func (s *recordedSink) Raise(_ context.Context, e Escalation) error {
	s.raised = append(s.raised, e)
	return nil
}

func TestStageFlowPostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "flow")
	caller := reviewer(t, pool, org, "owner", "flow-owner")
	project, err := store.CreateProject(ctx, org, "flow-project", "Flow project")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateFrozenRun(ctx, FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: "flow",
		Source: recipe.Input{Repo: guildRepo(t), Ref: "HEAD", Recipe: "examples/guild/recipe.json",
			Spec: "examples/guild/spec.md", Transcript: "examples/guild/transcript.md", Scope: "examples/guild"},
		Verification: VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{
			{ID: "project-tests", Command: []string{"go", "test", "./..."}},
			{ID: "requirement-coverage", Command: []string{"verify-coverage"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	collector := CommandExitCollector{Store: store, SignerID: "pool-one/connector", PublicKey: public, WorkerPool: "pool-one"}
	taskID := func(key string) string {
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2 AND task_key=$3`,
			org, run.ID, key).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	state := func(key string) string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT state FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2 AND task_key=$3`,
			org, run.ID, key).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	// complete drives one attempt through the store exactly as dispatch and
	// completion do, returning the frozen handoff it was launched with.
	inputs := map[string]string{} // stage -> input commit of its latest attempt
	complete := func(key string, result AttemptResult) string {
		t.Helper()
		task := taskID(key)
		handoff, corrections, err := store.BuildHandoff(ctx, org, run.ID, task)
		if err != nil {
			t.Fatal(err)
		}
		input, err := store.AttemptInput(ctx, org, run.ID, task)
		if err != nil {
			t.Fatal(err)
		}
		a, err := store.ReserveAttemptWithBinding(ctx, org, run.ID, task, func(ctx context.Context, tx pgx.Tx, a Attempt) error {
			if err := RecordHandoff(ctx, tx, a, handoff, corrections); err != nil {
				return err
			}
			return RecordInputCommit(ctx, tx, a, input)
		})
		if err != nil {
			t.Fatalf("reserve %s: %v", key, err)
		}
		if frozen, err := store.LoadHandoff(ctx, a); err != nil || frozen != handoff {
			t.Fatalf("handoff not frozen with attempt: %v", err)
		}
		if frozen, err := store.LoadInputCommit(ctx, a); err != nil || frozen != input {
			t.Fatalf("input commit not frozen with attempt: %q %v", frozen, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET input_commit=$3 WHERE organization_id=$1 AND id=$2`,
			org, a.ID, strings.Repeat("f", 40)); err == nil {
			t.Fatal("frozen input commit changed")
		}
		if target, err := store.GetRunBranchTarget(ctx, a); err != nil || target.Input != input ||
			target.CodeProducing != (key == "implement") {
			t.Fatalf("run branch target: %+v %v", target, err)
		}
		inputs[key] = input
		binding := RuntimeBinding{AXAtespace: "team", AXTask: "attempt-" + a.ID[:8], ActorUID: "actor-" + a.ID[:8],
			TemplateUID: "template", Image: "runner@sha256:" + strings.Repeat("a", 64),
			WorkerPool: "pool-one", CommandSHA256: strings.Repeat("b", 64)}
		if err := store.BindRuntime(ctx, a, binding); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmStarting(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmStarted(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordAttemptResult(ctx, a, result); !errors.Is(err, ErrConflict) {
			t.Fatalf("result accepted without a clean exit: %v", err)
		}
		var nonce [32]byte
		empty := sha256.Sum256(nil)
		signed, err := runnerexit.Sign(runnerexit.ExitReport{Schema: runnerexit.Schema, OrganizationID: org,
			RunID: run.ID, TaskID: task, AttemptID: a.ID, OwnerGeneration: a.OwnerGeneration,
			AXAtespace: binding.AXAtespace, AXTask: binding.AXTask, ActorUID: binding.ActorUID,
			TemplateUID: binding.TemplateUID, ActivationNonce: base64.RawURLEncoding.EncodeToString(nonce[:]),
			CommandSHA256: binding.CommandSHA256, Sequence: 1, EvidenceSHA256: hex.EncodeToString(empty[:]),
			ObservedAt: time.Now().UnixNano()}, private)
		if err != nil {
			t.Fatal(err)
		}
		if err := collector.Record(ctx, signed); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordAttemptResult(ctx, a, result); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmStopped(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmStopped(ctx, a); err != nil {
			t.Fatalf("accepted stop was not idempotent: %v", err)
		}
		return handoff
	}
	rev := func(c string) string { return strings.Repeat(c, 40) }

	if h := complete("plan", AttemptResult{Summary: "PLAN-SUMMARY"}); h != "" {
		t.Fatalf("root stage got a handoff: %q", h)
	}
	if state("plan") != "succeeded" || state("implement") != "pending" {
		t.Fatalf("plan acceptance did not advance: %s/%s", state("plan"), state("implement"))
	}
	if h := complete("implement", AttemptResult{Summary: "IMPL-1", Revision: rev("1")}); !strings.Contains(h, "Upstream stage plan") ||
		!strings.Contains(h, "PLAN-SUMMARY") || !strings.Contains(h, "Resulting revision: unchanged") {
		t.Fatalf("implement handoff: %q", h)
	}
	complete("review", AttemptResult{Summary: "looks fine"})
	// Downstream stages start from the accepted implement revision; the
	// implement stage itself (and plan) start from the run's source commit.
	if inputs["plan"] != run.SourceCommit || inputs["implement"] != run.SourceCommit || inputs["review"] != rev("1") {
		t.Fatalf("stage inputs: %+v (source %s)", inputs, run.SourceCommit)
	}
	// A failing loop stage sends its findings to a correction on implement.
	if h := complete("verify", AttemptResult{Summary: "FINDING-1", Verdict: "fail"}); !strings.Contains(h, "IMPL-1") || !strings.Contains(h, rev("1")) {
		t.Fatalf("verify handoff: %q", h)
	}
	for key, want := range map[string]string{"plan": "succeeded", "implement": "pending", "review": "pending", "verify": "pending", "architect-review": "pending"} {
		if got := state(key); got != want {
			t.Fatalf("after loop failure %s=%s, want %s", key, got, want)
		}
	}
	// A stage paced for model-gateway headroom waits until its retry time.
	if _, err := pool.Exec(ctx, `INSERT INTO gateway_paced_tasks (organization_id,task_id,run_id,pool_id,reason,retry_at)
		VALUES ($1,$2,$3,gen_random_uuid(),'Waiting for Claude production headroom',clock_timestamp()+interval '1 minute')`,
		org, taskID("implement"), run.ID); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.ListReadyTasks(ctx, org, 10); err != nil || len(ready) != 0 {
		t.Fatalf("paced stage dispatchable before its retry time: %+v, %v", ready, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE gateway_paced_tasks SET retry_at=clock_timestamp() WHERE organization_id=$1`, org); err != nil {
		t.Fatal(err)
	}
	if ready, err := store.ListReadyTasks(ctx, org, 10); err != nil || len(ready) != 1 || ready[0].Key != "implement" {
		t.Fatalf("correction was not dispatchable: %+v, %v", ready, err)
	}
	if h := complete("implement", AttemptResult{Summary: "IMPL-2", Revision: rev("2")}); !strings.Contains(h, "Correction requested by verify") ||
		!strings.Contains(h, "FINDING-1") {
		t.Fatalf("correction handoff: %q", h)
	}
	if h, corrections, err := store.BuildHandoff(ctx, org, run.ID, taskID("implement")); err != nil || len(corrections) != 0 || strings.Contains(h, "FINDING-1") {
		t.Fatalf("consumed correction reappeared: %q %v %v", h, corrections, err)
	}
	complete("review", AttemptResult{Summary: "still fine"})
	complete("verify", AttemptResult{Summary: "all checks pass", Verdict: "pass"})
	if h := complete("architect-review", AttemptResult{Summary: "approved"}); !strings.Contains(h, "Upstream stage review") ||
		!strings.Contains(h, "Upstream stage verify") {
		t.Fatalf("architect handoff: %q", h)
	}
	// Fan-in: architect-review reads the implement revision through review/verify.
	if inputs["architect-review"] != rev("2") || inputs["verify"] != rev("2") {
		t.Fatalf("fan-in input: %+v", inputs)
	}
	batch, err := store.Progress(ctx, "", "", 10, nil)
	if err != nil || batch.Finalized != 1 || batch.Presented != 1 {
		t.Fatalf("progress did not present completed run: %+v, %v", batch, err)
	}
	pkg, err := store.GetCurrentReview(ctx, org, run.ID)
	if err != nil || pkg.IntegratedCommit != rev("2") {
		t.Fatalf("review package: %+v, %v", pkg, err)
	}
	// Human request_changes reopens the run with a correction on implement.
	if _, err := store.DecideReview(ctx, caller, run.ID, pkg.ID, "changes-1", "request_changes", "Please rename the flag."); err != nil {
		t.Fatal(err)
	}
	if batch, err := store.Progress(ctx, "", "", 10, nil); err != nil || batch.Corrected != 1 {
		t.Fatalf("request_changes not applied: %+v, %v", batch, err)
	}
	if again, err := store.Progress(ctx, "", "", 10, nil); err != nil || again.Examined != 0 {
		t.Fatalf("applied decision was reprocessed: %+v, %v", again, err)
	}
	current, err := store.GetRun(ctx, org, run.ID)
	if err != nil || current.State != "active" || state("implement") != "pending" || state("architect-review") != "pending" {
		t.Fatalf("run not reopened: %+v %s, %v", current, state("implement"), err)
	}
	if h := complete("implement", AttemptResult{Summary: "IMPL-3", Revision: rev("3")}); !strings.Contains(h, "Correction requested by human-review") ||
		!strings.Contains(h, "Please rename the flag.") {
		t.Fatalf("human correction handoff: %q", h)
	}
	// Exhaust the verify loop: max_cycles=3 and one correction is already used.
	complete("review", AttemptResult{Summary: "fine"})
	for i := 0; i < 2; i++ {
		complete("verify", AttemptResult{Summary: "FAIL", Verdict: "fail"})
		complete("implement", AttemptResult{Summary: "fix", Revision: rev("4")})
		complete("review", AttemptResult{Summary: "fine"})
	}
	complete("verify", AttemptResult{Summary: "STILL-FAILING", Verdict: "fail"})
	if state("verify") != "escalated" || state("implement") != "succeeded" {
		t.Fatalf("loop cap did not escalate: verify=%s implement=%s", state("verify"), state("implement"))
	}
	sink := &recordedSink{}
	if batch, err := store.Progress(ctx, "", "", 10, sink); err != nil || batch.Raised != 1 || len(sink.raised) != 1 ||
		sink.raised[0].StageKey != "verify" || sink.raised[0].BodyMD != "STILL-FAILING" || len(sink.raised[0].Options) != 3 {
		t.Fatalf("escalation not raised: %+v %+v, %v", batch, sink.raised, err)
	}
	if batch, err := store.Progress(ctx, "", "", 10, sink); err != nil || len(sink.raised) != 1 || batch.Examined != 0 {
		t.Fatalf("escalation raised twice: %+v, %v", batch, err)
	}
	graph, err := store.ListRunTasks(ctx, org, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range graph {
		if task.Key == "verify" && (task.Kind != "verify" || task.LoopWith != "implement" || task.MaxCycles != 3 || task.LoopCycles != 3) {
			t.Fatalf("loop read model: %+v", task)
		}
		if task.Key == "plan" && (task.Kind != "plan" || task.LoopWith != "") {
			t.Fatalf("stage kind read model: %+v", task)
		}
	}
	key := sink.raised[0].Key
	if err := store.ResolveEscalation(ctx, org, run.ID, key, "take_over", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("take_over treated as an engine action: %v", err)
	}
	if err := store.ResolveEscalation(ctx, org, run.ID, key, "raise_cap", 1); err != nil {
		t.Fatal(err)
	}
	if err := store.ResolveEscalation(ctx, org, run.ID, key, "halt", 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("resolved escalation changed: %v", err)
	}
	if state("implement") != "pending" || state("verify") != "pending" {
		t.Fatalf("raised cap did not correct: %s/%s", state("implement"), state("verify"))
	}
	if h := complete("implement", AttemptResult{Summary: "fixed", Revision: rev("5")}); !strings.Contains(h, "STILL-FAILING") {
		t.Fatalf("escalation findings missing from handoff: %q", h)
	}
}

// guildRepo commits the working-tree Guild example so the test freezes the
// recipe under review, not the checkout's HEAD.
func guildRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source, err := filepath.Abs("../../examples/guild")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		target := filepath.Join(root, "examples/guild", rel)
		data, err := os.ReadFile(path)
		if err == nil {
			err = os.MkdirAll(filepath.Dir(target), 0o755)
		}
		if err == nil {
			err = os.WriteFile(target, data, 0o644)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "."},
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "guild"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(root, ".git"), "GIT_WORK_TREE="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}
