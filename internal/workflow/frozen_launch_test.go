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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
)

func TestFrozenLaunchPostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "frozen")
	project, err := store.CreateProject(t.Context(), org, "frozen-project", "Frozen project")
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	in := FrozenRunInput{
		OrganizationID: org, ProjectID: project, LaunchKey: "same-request",
		Source: recipe.Input{Repo: filepath.Join(wd, "../.."), Ref: "HEAD",
			Recipe: "examples/guild/recipe.json", Spec: "examples/guild/spec.md",
			Transcript: "examples/guild/transcript.md", Scope: "examples/guild"},
		Verification: VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{
			{ID: "project-tests", Command: []string{"go", "test", "./..."}},
			{ID: "requirement-coverage", Command: []string{"verify-coverage"}},
		}},
	}
	run, err := store.CreateFrozenRun(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	var sealed bool
	var tasks, deps int
	if err := pool.QueryRow(t.Context(), `SELECT graph_sealed FROM workflow_runs
		WHERE organization_id=$1 AND id=$2`, org, run.ID).Scan(&sealed); err != nil || !sealed {
		t.Fatalf("run did not seal: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_tasks
		WHERE organization_id=$1 AND run_id=$2`, org, run.ID).Scan(&tasks); err != nil || tasks != 5 {
		t.Fatalf("wrong task count %d: %v", tasks, err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_task_dependencies
		WHERE organization_id=$1 AND run_id=$2`, org, run.ID).Scan(&deps); err != nil || deps != 5 {
		t.Fatalf("wrong dependency count %d: %v", deps, err)
	}
	var bundle, verification []byte
	if err := pool.QueryRow(t.Context(), `SELECT bundle_json,verification_json FROM workflow_run_bundles
		WHERE organization_id=$1 AND run_id=$2`, org, run.ID).Scan(&bundle, &verification); err != nil || len(bundle) == 0 || len(verification) == 0 {
		t.Fatalf("frozen inputs missing: %v", err)
	}
	again, err := store.CreateFrozenRun(t.Context(), in)
	if err != nil || again.ID != run.ID {
		t.Fatalf("idempotent replay: %+v, %v", again, err)
	}
	in.Verification.Checks[0].Command = []string{"go", "test", "./internal/..."}
	if _, err := store.CreateFrozenRun(t.Context(), in); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed verification policy replay accepted: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO workflow_tasks
		(organization_id,run_id,task_key,input_sha256,max_attempts)
		VALUES ($1,$2,'late',$3,1)`, org, run.ID, run.BundleSHA256); err == nil {
		t.Fatal("sealed graph accepted another task")
	}
	var firstTask string
	if err := pool.QueryRow(t.Context(), `SELECT id FROM workflow_tasks
		WHERE organization_id=$1 AND run_id=$2 AND task_key='plan'`, org, run.ID).Scan(&firstTask); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(context.Background(), org, run.ID, firstTask)
	if err != nil {
		t.Fatalf("sealed root task could not reserve: %v", err)
	}
	binding := RuntimeBinding{AXAtespace: "team", AXTask: "attempt", ActorUID: "actor-one",
		TemplateUID: "template-one", Image: "runner@sha256:" + strings.Repeat("a", 64),
		WorkerPool: "pool-one", CommandSHA256: strings.Repeat("b", 64)}
	if err := store.BindRuntime(t.Context(), attempt, binding); err != nil {
		t.Fatal(err)
	}
	if err := store.BindRuntime(t.Context(), attempt, binding); err != nil {
		t.Fatalf("same runtime binding was not idempotent: %v", err)
	}
	binding.ActorUID = "different-actor"
	if err := store.BindRuntime(t.Context(), attempt, binding); !errors.Is(err, ErrConflict) {
		t.Fatalf("runtime identity changed: %v", err)
	}
	binding.ActorUID = "actor-one"
	if err := store.ConfirmStarted(t.Context(), attempt); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	collector := CommandExitCollector{Store: store, SignerID: "pool-one/connector", PublicKey: public, WorkerPool: "pool-one"}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256(nil)
	report := runnerexit.ExitReport{Schema: runnerexit.Schema, OrganizationID: org,
		RunID: run.ID, TaskID: firstTask, AttemptID: attempt.ID, OwnerGeneration: attempt.OwnerGeneration,
		AXAtespace: binding.AXAtespace, AXTask: binding.AXTask, ActorUID: binding.ActorUID,
		TemplateUID: binding.TemplateUID, ActivationNonce: base64.RawURLEncoding.EncodeToString(nonce[:]),
		CommandSHA256: binding.CommandSHA256, ExitCode: 0, Sequence: 1,
		EvidenceSHA256: hex.EncodeToString(empty[:]), ObservedAt: time.Now().UnixNano()}
	signed, err := runnerexit.Sign(report, private)
	if err != nil {
		t.Fatal(err)
	}
	wrong := signed
	wrong.Report.ActorUID = "other-actor"
	if err := collector.Record(t.Context(), wrong); !errors.Is(err, ErrInvalid) {
		t.Fatalf("altered signed report accepted: %v", err)
	}
	wrong, err = runnerexit.Sign(wrong.Report, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.Record(t.Context(), wrong); !errors.Is(err, ErrFenced) {
		t.Fatalf("wrong actor accepted: %v", err)
	}
	if err := collector.Record(t.Context(), signed); err != nil {
		t.Fatal(err)
	}
	if err := collector.Record(t.Context(), signed); err != nil {
		t.Fatalf("same signed receipt was not idempotent: %v", err)
	}
	report.ExitCode = 1
	wrong, err = runnerexit.Sign(report, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.Record(t.Context(), wrong); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed receipt replaced first report: %v", err)
	}
	var state string
	if err := pool.QueryRow(t.Context(), `SELECT state FROM workflow_tasks WHERE organization_id=$1 AND id=$2`,
		org, firstTask).Scan(&state); err != nil || state != "running" {
		t.Fatalf("command exit implied task success: %q %v", state, err)
	}
}

func TestVerificationPolicyRequiresTrustedChecks(t *testing.T) {
	policy := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{
		{ID: "project-tests", Command: []string{"go", "test", "./..."}},
	}}
	if _, err := validateVerification(policy, []string{"project-tests", "requirement-coverage"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing required check accepted: %v", err)
	}
	policy.Checks = append(policy.Checks, VerificationCheck{ID: "project-tests", Command: []string{"true"}})
	if _, err := validateVerification(policy, []string{"project-tests"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate check accepted: %v", err)
	}
}
