package axbridge

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type testActivation struct {
	verify func(bootstrap.Scope, bootstrap.Runtime, string) error
}

type completionActor struct {
	fakeActor
	gone *bool
}

func (a completionActor) Gone(context.Context, string, string) (bool, error) { return *a.gone, nil }

func (t testActivation) VerifyActivation(_ context.Context, scope bootstrap.Scope, runtime bootstrap.Runtime, nonce string) error {
	return t.verify(scope, runtime, nonce)
}

func TestCommandExitReaderRequiresCurrentActorRoute(t *testing.T) {
	called := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		if r.URL.Path != "/blaxsmith/command-exit" || r.Method != http.MethodGet ||
			r.Header.Get("Authorization") != "Bearer token" ||
			r.Header.Get("ate-target-actor") != "space/actor" ||
			r.Header.Get("X-Blaxsmith-Actor-UID") != "uid" {
			t.Errorf("readback request lacked current actor routing: %+v", r.Header)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	reader := CommandExitReader{Client: server.Client(), RouterURL: server.URL,
		Token: func(context.Context) (string, error) { return "token", nil }}
	actor := bootstrap.Actor{Atespace: "space", Name: "actor", UID: "uid"}
	if _, err := reader.read(t.Context(), actor); !errors.Is(err, ErrPending) || called != 1 {
		t.Fatalf("pending readback = %v, calls=%d", err, called)
	}
	reader.RouterURL = "http://insecure.example"
	if _, err := reader.read(t.Context(), actor); !errors.Is(err, workflow.ErrInvalid) || called != 1 {
		t.Fatalf("plaintext route accepted: %v", err)
	}
	redirected := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected = true }))
	defer target.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	reader.RouterURL = redirect.URL
	reader.Client = redirect.Client()
	if _, err := reader.read(t.Context(), actor); !errors.Is(err, ErrMismatch) || redirected {
		t.Fatalf("redirect followed or accepted: %v, redirected=%v", err, redirected)
	}
}

func TestCommandExitConnectorRecordsOnlyBoundObservation(t *testing.T) {
	for _, exitCode := range []int{0, 7} {
		pool := bridgePool(t)
		store, err := workflow.New(pool)
		if err != nil {
			t.Fatal(err)
		}
		var org string
		if err := pool.QueryRow(t.Context(), `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'exit-org','Exit Org') RETURNING id`).Scan(&org); err != nil {
			t.Fatal(err)
		}
		project, err := store.CreateProject(t.Context(), org, "exit-project", "Exit Project")
		if err != nil {
			t.Fatal(err)
		}
		run, err := store.CreateRun(t.Context(), workflow.RunInput{OrganizationID: org, ProjectID: project,
			LaunchKey: "exit-run", SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
		if err != nil {
			t.Fatal(err)
		}
		taskID, err := store.AddTask(t.Context(), org, run.ID, "implement", strings.Repeat("d", 64), 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
			t.Fatal(err)
		}
		attempt, err := store.ReserveAttempt(t.Context(), org, run.ID, taskID)
		if err != nil {
			t.Fatal(err)
		}
		space, name := Name(attempt)
		image := "runner@sha256:" + strings.Repeat("e", 64)
		runtime := bootstrap.Runtime{Actor: bootstrap.Actor{Atespace: space, Name: name, UID: "actor-uid"},
			TemplateUID: "template-uid", Image: image, WorkerPool: "pool-a", WorkerPod: "pod", WorkerPodUID: "pod-uid",
			SandboxClass: "SANDBOX_CLASS_GVISOR", BootstrapPublicKey: "bootstrap-key",
			SnapshotOnPause: "SNAPSHOT_CONTENT_SCOPE_DATA", SnapshotOnCommit: "SNAPSHOT_CONTENT_SCOPE_DATA",
			ResumeFromData: "RESUME_SOURCE_GOLDEN", SnapshotStorage: "gs://snapshots/"}
		ax := &fakeAX{}
		gone := false
		bridge := &Bridge{Workflow: store, AX: ax, Actor: completionActor{fakeActor{runtime}, &gone}, Image: image,
			Pool: "pool-a", Signer: "bootstrap-key", Storage: "gs://snapshots/"}
		task, err := bridge.task(attempt)
		if err != nil {
			t.Fatal(err)
		}
		task.Status.Phase, task.Status.Actor = "Running", name
		ax.task = &task
		if err := store.BindRuntime(t.Context(), attempt, workflow.RuntimeBinding{AXAtespace: space, AXTask: name,
			ActorUID: runtime.Actor.UID, TemplateUID: runtime.TemplateUID, Image: image, WorkerPool: "pool-a",
			CommandSHA256: runnerexit.CommandSHA256(syntheticCommand)}); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmStarting(t.Context(), attempt); err != nil {
			t.Fatal(err)
		}
		if err := store.ConfirmStarted(t.Context(), attempt); err != nil {
			t.Fatal(err)
		}
		var nonceBytes [32]byte
		if _, err := rand.Read(nonceBytes[:]); err != nil {
			t.Fatal(err)
		}
		nonce := base64.RawURLEncoding.EncodeToString(nonceBytes[:])
		observation := exitReadback{Schema: exitReadbackSchema, ActivationNonce: nonce,
			AXAtespace: space, AXTask: name, CommandSHA256: runnerexit.CommandSHA256(syntheticCommand),
			ExitCode: exitCode, Sequence: 1, ObservedAt: 123}
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(observation)
		}))
		defer server.Close()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		proved := 0
		connector := CommandExitConnector{Bridge: bridge,
			Reader: &CommandExitReader{Client: server.Client(), RouterURL: server.URL,
				Token: func(context.Context) (string, error) { return "token", nil }},
			ClusterID: "cluster-a", Signer: private,
			Activation: testActivation{func(scope bootstrap.Scope, got bootstrap.Runtime, n string) error {
				proved++
				if scope.AttemptID != attempt.ID || scope.OwnerGeneration != attempt.OwnerGeneration || got != runtime || n != nonce {
					return bootstrap.ErrDenied
				}
				return nil
			}},
			Collector: workflow.CommandExitCollector{Store: store, SignerID: "pool-a/connector", PublicKey: public, WorkerPool: "pool-a"}}
		observation.CommandSHA256 = strings.Repeat("f", 64)
		if err := connector.Collect(t.Context(), attempt); !errors.Is(err, ErrMismatch) || proved != 0 {
			t.Fatalf("wrong command accepted: %v", err)
		}
		observation.CommandSHA256 = runnerexit.CommandSHA256(syntheticCommand)
		connector.Activation = testActivation{func(bootstrap.Scope, bootstrap.Runtime, string) error { return bootstrap.ErrDenied }}
		if err := connector.Collect(t.Context(), attempt); !errors.Is(err, bootstrap.ErrDenied) {
			t.Fatalf("unproved activation accepted: %v", err)
		}
		connector.Activation = testActivation{func(scope bootstrap.Scope, got bootstrap.Runtime, n string) error {
			proved++
			if scope.AttemptID != attempt.ID || scope.OwnerGeneration != attempt.OwnerGeneration || got != runtime || n != nonce {
				return bootstrap.ErrDenied
			}
			return nil
		}}
		if err := connector.Collect(t.Context(), attempt); err != nil {
			t.Fatal(err)
		}
		if err := connector.Collect(t.Context(), attempt); err != nil {
			t.Fatalf("same readback was not idempotent: %v", err)
		}
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_command_exits WHERE organization_id=$1 AND attempt_id=$2`, org, attempt.ID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("exit receipt = %d, %v", count, err)
		}
		if _, state, _, err := store.CurrentAttempt(t.Context(), attempt); err != nil || state != "running" {
			t.Fatalf("exit alone finished task: %s, %v", state, err)
		}
		connector.Collector.PublicKey = make(ed25519.PublicKey, ed25519.PublicKeySize)
		if err := connector.Reconcile(t.Context(), attempt); !errors.Is(err, workflow.ErrConflict) {
			t.Fatalf("receipt with wrong signing key was trusted: %v", err)
		}
		connector.Collector.PublicKey = public
		running, err := store.ListRunningAttempts(t.Context(), "", "", 1)
		if err != nil || len(running) != 1 || running[0].ID != attempt.ID {
			t.Fatalf("running attempt scan = %+v, %v", running, err)
		}
		revoked := 0
		bridge.RevokeOwner = func(context.Context, workflow.Attempt) error { revoked++; return nil }
		sweep := CompletionSweep{Workflow: store, Connector: func(_ context.Context, found workflow.Attempt) (*CommandExitConnector, error) {
			if found.ID != attempt.ID || found.OwnerGeneration != attempt.OwnerGeneration || found.FenceToken != attempt.FenceToken {
				t.Fatalf("sweep lost frozen attempt ownership: %+v", found)
			}
			return &connector, nil
		}}
		batch, err := sweep.Sweep(t.Context(), "", "", 1)
		if err != nil || batch.Examined != 1 || batch.AfterAttemptID != attempt.ID {
			t.Fatalf("completion sweep did not advance cursor: %+v, %v", batch, err)
		}
		running, err = store.ListRunningAttempts(t.Context(), "", "", 1)
		if err != nil {
			t.Fatal(err)
		}
		if exitCode == 0 {
			if batch.VerificationPending != 1 || batch.Stopped != 0 || revoked != 0 || len(running) != 1 {
				t.Fatalf("clean exit falsely completed without verification: %+v, running=%d, revoked=%d", batch, len(running), revoked)
			}
		} else {
			if batch.Waiting != 1 || batch.Stopped != 0 || revoked != 1 || len(running) != 1 || ax.task != nil {
				t.Fatalf("failed exit skipped actor-gone proof: %+v, running=%d, revoked=%d", batch, len(running), revoked)
			}
			gone = true
			batch, err = sweep.Sweep(t.Context(), "", "", 1)
			if err != nil || batch.Stopped != 1 || revoked != 2 {
				t.Fatalf("durable signed receipt did not resume stop: %+v, revoked=%d, %v", batch, revoked, err)
			}
			running, err = store.ListRunningAttempts(t.Context(), "", "", 1)
			if err != nil || len(running) != 0 {
				t.Fatalf("stopped attempt remained in scan: %+v, %v", running, err)
			}
			if _, _, _, err := store.CurrentAttempt(t.Context(), attempt); !errors.Is(err, workflow.ErrFenced) {
				t.Fatalf("stopped attempt still owns task: %v", err)
			}
		}
	}
}
