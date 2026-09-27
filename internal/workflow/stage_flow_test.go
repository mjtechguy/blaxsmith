package workflow

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/guild"
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
		Source: recipe.Input{Validators: map[string]recipe.Validator{"guild-forge": guild.ValidateInputs}, Repo: guildRepo(t), Ref: "HEAD", Recipe: "examples/guild/recipe.json",
			Scope: "examples/guild"},
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
		if key == "review" || key == "architect-review" || key == "ui-review" {
			result.Revision = input
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
		if key == "verify" {
			if allowed, err := store.CanTakeOverAttempt(ctx, caller, a.ID); err != nil || allowed {
				t.Fatalf("verify takeover advertised: %v %v", allowed, err)
			}
			if _, _, err := store.TakeOverAttempt(ctx, caller, a.ID); !errors.Is(err, ErrAttemptControlDenied) {
				t.Fatalf("verify takeover accepted: %v", err)
			}
			if claimed, err := store.ClaimVerification(ctx, a); err != nil || !claimed {
				t.Fatalf("verification claim: %v %v", claimed, err)
			}
			code := 0
			if result.Verdict != "pass" {
				code = 1
			}
			checks := []CheckObservation{{Check: "project-tests", ExitCode: code, Verdict: result.Verdict, Summary: result.Summary}, {Check: "requirement-coverage", ExitCode: code, Verdict: result.Verdict, Summary: result.Summary}}
			if err := store.RecordVerification(ctx, a, strings.Repeat("f", 40), checks, [][]byte{nil, nil}); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale verification revision accepted: %v", err)
			}
			if claimed, err := store.ClaimVerification(ctx, a); err != nil || claimed {
				t.Fatalf("verification was claimed twice: %v %v", claimed, err)
			}
			outputs := [][]byte{nil, nil}
			if result.Verdict == "fail" {
				outputs[0] = []byte("FAIL: TestRepair\nrepair_test.go:42: expected recovered correction")
			}
			if err := store.RecordVerification(ctx, a, input, checks, outputs); err != nil {
				t.Fatal(err)
			}
		} else if err := store.RecordAttemptResult(ctx, a, result); err != nil {
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
	complete("review", AttemptResult{Summary: "looks fine", Verdict: "pass"})
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
	if ready, err := store.ListReadyTasks(ctx, org, 10); err != nil || len(ready) != 1 || ready[0].Key != "implement" {
		t.Fatalf("correction was not dispatchable: %+v, %v", ready, err)
	}
	// A worker lost after reservation must not swallow its correction request.
	repairTask := taskID("implement")
	handoff, correctionIDs, err := store.BuildHandoff(ctx, org, run.ID, repairTask)
	if err != nil || len(correctionIDs) != 1 || !strings.Contains(handoff, "repair_test.go:42") || !strings.Contains(handoff, "Command argv:") {
		t.Fatalf("repair diagnostics missing: %q %v", handoff, err)
	}
	interrupted, err := store.ReserveAttemptWithBinding(ctx, org, run.ID, repairTask, func(ctx context.Context, tx pgx.Tx, a Attempt) error {
		return RecordHandoff(ctx, tx, a, handoff, correctionIDs)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkUnknown(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if _, ids, err := store.BuildHandoff(ctx, org, run.ID, repairTask); err != nil || len(ids) != 0 {
		t.Fatalf("live correction reassigned: %v %v", ids, err)
	}
	if err := store.ConfirmStopped(ctx, interrupted); err != nil {
		t.Fatal(err)
	}
	if retry, ids, err := store.BuildHandoff(ctx, org, run.ID, repairTask); err != nil || len(ids) != 1 || retry != handoff {
		t.Fatalf("retry lost frozen findings: %q %v %v", retry, ids, err)
	}
	if h := complete("implement", AttemptResult{Summary: "IMPL-2", Revision: rev("2")}); !strings.Contains(h, "Correction requested by verify") ||
		!strings.Contains(h, "FINDING-1") {
		t.Fatalf("correction handoff: %q", h)
	}
	if inputs["implement"] != rev("1") {
		t.Fatalf("repair restarted from %s instead of its accepted revision", inputs["implement"])
	}
	if original, err := store.LoadHandoff(ctx, interrupted); err != nil || original != handoff {
		t.Fatalf("retry rewrote original handoff: %q %v", original, err)
	}
	if h, corrections, err := store.BuildHandoff(ctx, org, run.ID, taskID("implement")); err != nil || len(corrections) != 0 || strings.Contains(h, "FINDING-1") {
		t.Fatalf("consumed correction reappeared: %q %v %v", h, corrections, err)
	}
	complete("review", AttemptResult{Summary: "still fine", Verdict: "pass"})
	complete("verify", AttemptResult{Summary: "all checks pass", Verdict: "pass"})
	if h := complete("architect-review", AttemptResult{Summary: "approved", Verdict: "pass"}); !strings.Contains(h, "Upstream stage review") ||
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
	complete("review", AttemptResult{Summary: "fine", Verdict: "pass"})
	for i := 0; i < 2; i++ {
		complete("verify", AttemptResult{Summary: "FAIL", Verdict: "fail"})
		complete("implement", AttemptResult{Summary: "fix", Revision: rev("4")})
		complete("review", AttemptResult{Summary: "fine", Verdict: "pass"})
	}
	complete("verify", AttemptResult{Summary: "STILL-FAILING", Verdict: "fail"})
	if state("verify") != "escalated" || state("implement") != "succeeded" {
		t.Fatalf("loop cap did not escalate: verify=%s implement=%s", state("verify"), state("implement"))
	}
	sink := &recordedSink{}
	if batch, err := store.Progress(ctx, "", "", 10, sink); err != nil || batch.Raised != 1 || len(sink.raised) != 1 ||
		sink.raised[0].StageKey != "verify" || !strings.Contains(sink.raised[0].BodyMD, "STILL-FAILING") || len(sink.raised[0].Options) != 3 {
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

func TestVerificationRepairSummary(t *testing.T) {
	var policy []VerificationCheck
	var checks []CheckObservation
	var outputs [][]byte
	for i := range 64 {
		id := fmt.Sprintf("check-%02d", i)
		policy = append(policy, VerificationCheck{ID: id, Command: []string{"test", id}})
		checks = append(checks, CheckObservation{Check: id, Verdict: "fail", ExitCode: 1, Summary: "Command failed"})
		outputs = append(outputs, []byte("START\x00\xff"+strings.Repeat("é", 32000)+"FINAL-DIAGNOSTIC"))
	}
	summary := verificationSummary(strings.Repeat("a", 40), policy, checks, outputs)
	if len(summary) > maxSummaryBytes || !utf8.ValidString(summary) || strings.ContainsRune(summary, 0) {
		t.Fatalf("invalid summary: %d bytes", len(summary))
	}
	for _, check := range checks {
		if !strings.Contains(summary, check.Check+": fail (exit 1)") {
			t.Fatalf("missing status: %s", check.Check)
		}
	}
	if strings.Count(summary, "FINAL-DIAGNOSTIC") != 64 {
		t.Fatal("later check output was starved")
	}
	summary = verificationSummary(strings.Repeat("a", 40), policy[:1], checks[:1], outputs[:1])
	if !strings.Contains(summary, "START�") || !strings.Contains(summary, "FINAL-DIAGNOSTIC") || !strings.Contains(summary, "truncated") {
		t.Fatalf("lost output boundaries: %q", summary)
	}
}

func TestHandoffPrioritizesCorrectionsPostgres(t *testing.T) {
	pool := testPool(t)
	s, _ := New(pool)
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "handoff-budget")
	project, err := s.CreateProject(ctx, org, "handoff-budget", "Handoff budget")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "budget", SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.AddTask(ctx, org, run.ID, "repair", run.BundleSHA256, 2)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		parent, err := s.AddTask(ctx, org, run.ID, fmt.Sprintf("parent-%d", i), run.BundleSHA256, 2)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workflow_task_dependencies(organization_id,run_id,task_id,depends_on_task_id) VALUES($1,$2,$3,$4)`, org, run.ID, task, parent); err != nil {
			t.Fatal(err)
		}
		var attempt string
		if err := pool.QueryRow(ctx, `INSERT INTO workflow_attempts(organization_id,run_id,task_id,generation,state) VALUES($1,$2,$3,1,'succeeded') RETURNING id`, org, run.ID, parent).Scan(&attempt); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workflow_attempt_results(organization_id,run_id,task_id,attempt_id,summary,verdict,result_sha256) VALUES($1,$2,$3,$4,$5,'pass',$6)`, org, run.ID, parent, attempt, strings.Repeat("é", maxSummaryBytes/2), sha(nil)); err != nil {
			t.Fatal(err)
		}
	}
	request := "MUST-REPAIR-THIS " + strings.Repeat("x", maxSummaryBytes-32) + " END-REQUEST"
	addRequest := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workflow_corrections(organization_id,run_id,task_id,source_stage,message) VALUES($1,$2,$3,'verify',$4)`, org, run.ID, task, request); err != nil {
			t.Fatal(err)
		}
	}
	addRequest()
	handoff, ids, err := s.BuildHandoff(ctx, org, run.ID, task)
	if err != nil || len(ids) != 1 || len(handoff) > maxHandoffBytes || !utf8.ValidString(handoff) || !strings.Contains(handoff, request) || !strings.Contains(handoff, "Upstream context truncated") {
		t.Fatalf("correction lost to upstream context: %d bytes, %v, %v", len(handoff), ids, err)
	}
	for range 4 {
		addRequest()
	}
	if h, ids, err := s.BuildHandoff(ctx, org, run.ID, task); !errors.Is(err, ErrInvalid) || h != "" || len(ids) != 0 {
		t.Fatalf("oversized corrections silently consumed: %d bytes, %v, %v", len(h), ids, err)
	}
}

func anvilRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	source, err := filepath.Abs("../../examples/anvil")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(source, path)
		target := filepath.Join(root, "examples/anvil", rel)
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
		{"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-qm", "anvil"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_DIR="+filepath.Join(root, ".git"), "GIT_WORK_TREE="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}
