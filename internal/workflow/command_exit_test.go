package workflow

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestRuntimeBoundSuccessRequiresCleanExitPostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "exit-gate")
	project, err := store.CreateProject(tenant.System(t.Context()), org, "exit-gate", "Exit gate")
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	collector := CommandExitCollector{Store: store, SignerID: "pool-one/connector", PublicKey: public, WorkerPool: "pool-one"}
	for _, test := range []struct {
		name        string
		receipt     bool
		exitCode    int
		interrupted bool
		allow       bool
	}{
		{name: "missing", receipt: false},
		{name: "nonzero", receipt: true, exitCode: 7},
		{name: "interrupted", receipt: true, interrupted: true},
		{name: "clean", receipt: true, allow: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := tenant.System(t.Context())
			run, err := store.CreateRun(ctx, RunInput{OrganizationID: org, ProjectID: project,
				LaunchKey: test.name, SourceCommit: strings.Repeat("a", 40),
				BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
			if err != nil {
				t.Fatal(err)
			}
			task, err := store.AddTask(ctx, org, run.ID, "implement", run.BundleSHA256, 1)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true
				WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
				t.Fatal(err)
			}
			attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
			if err != nil {
				t.Fatal(err)
			}
			binding := RuntimeBinding{AXAtespace: "team", AXTask: "attempt-" + test.name,
				ActorUID: "actor-one", TemplateUID: "template-one",
				Image: "runner@sha256:" + strings.Repeat("a", 64), WorkerPool: "pool-one",
				CommandSHA256: strings.Repeat("b", 64)}
			if err := store.BindRuntime(ctx, attempt, binding); err != nil {
				t.Fatal(err)
			}
			if err := store.ConfirmStarting(ctx, attempt); err != nil {
				t.Fatal(err)
			}
			if err := store.ConfirmStarted(ctx, attempt); err != nil {
				t.Fatal(err)
			}
			if test.receipt {
				var nonce [32]byte
				if _, err := rand.Read(nonce[:]); err != nil {
					t.Fatal(err)
				}
				empty := sha256.Sum256(nil)
				report := runnerexit.ExitReport{Schema: runnerexit.Schema, OrganizationID: org,
					RunID: run.ID, TaskID: task, AttemptID: attempt.ID, OwnerGeneration: attempt.OwnerGeneration,
					AXAtespace: binding.AXAtespace, AXTask: binding.AXTask, ActorUID: binding.ActorUID,
					TemplateUID: binding.TemplateUID, ActivationNonce: base64.RawURLEncoding.EncodeToString(nonce[:]),
					CommandSHA256: binding.CommandSHA256, ExitCode: test.exitCode,
					Interrupted: test.interrupted, Sequence: 1,
					EvidenceSHA256: hex.EncodeToString(empty[:]), ObservedAt: time.Now().UnixNano()}
				signed, err := runnerexit.Sign(report, private)
				if err != nil {
					t.Fatal(err)
				}
				if err := collector.Record(ctx, signed); err != nil {
					t.Fatal(err)
				}
			}
			err = store.FinishAttempt(ctx, attempt, true, strings.Repeat("d", 64))
			if test.allow && err != nil || !test.allow && !errors.Is(err, ErrConflict) {
				t.Fatalf("finish after %s exit: %v", test.name, err)
			}
			var state string
			if err := pool.QueryRow(ctx, `SELECT state FROM workflow_tasks WHERE organization_id=$1 AND id=$2`, org, task).Scan(&state); err != nil {
				t.Fatal(err)
			}
			want := "running"
			if test.allow {
				want = "succeeded"
			}
			if state != want {
				t.Fatalf("task state after %s exit = %q, want %q", test.name, state, want)
			}
		})
	}
}
