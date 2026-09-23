package axbridge

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type fakeAX struct {
	task     *Task
	applyErr error
	write    bool
}

func (f *fakeAX) Get(_ context.Context, _, _ string) (Task, error) {
	if f.task == nil {
		return Task{}, ErrNotFound
	}
	return *f.task, nil
}
func (f *fakeAX) Apply(_ context.Context, task Task) error {
	if f.write {
		f.task = &task
	}
	return f.applyErr
}
func (f *fakeAX) Delete(_ context.Context, _, _ string) error {
	f.task = nil
	return nil
}

type fakeActor struct{ runtime bootstrap.Runtime }

func (f fakeActor) Current(context.Context, string, string) (bootstrap.Runtime, error) {
	return f.runtime, nil
}
func (f fakeActor) Gone(context.Context, string, string) (bool, error) { return true, nil }

func TestAttemptDispatchAndUncertainReadBack(t *testing.T) {
	pool := bridgePool(t)
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	var org string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'bridge-org','Bridge Org') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, org, "bridge-project", "Bridge Project")
	if err != nil {
		t.Fatal(err)
	}
	reserve := func(key string) workflow.Attempt {
		t.Helper()
		run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: org, ProjectID: project, LaunchKey: key,
			SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
		if err != nil {
			t.Fatal(err)
		}
		task, err := store.AddTask(ctx, org, run.ID, "implement", strings.Repeat("d", 64), 2)
		if err != nil {
			t.Fatal(err)
		}
		// This bridge test constructs a synthetic sealed fixture; the product
		// launch path seals a real frozen recipe through CreateFrozenRun.
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true
			WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
			t.Fatal(err)
		}
		a, err := store.ReserveAttempt(ctx, org, run.ID, task)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	image := "registry.example/ax-task-runner@sha256:" + strings.Repeat("e", 64)
	makeBridge := func(a workflow.Attempt, ax *fakeAX) *Bridge {
		space, name := Name(a)
		return &Bridge{Workflow: store, AX: ax, Image: image, Pool: "pool-a", Signer: "synthetic-key", Storage: "gs://snapshots/test/",
			Actor: fakeActor{bootstrap.Runtime{Actor: bootstrap.Actor{Atespace: space, Name: name, UID: "actor-uid"},
				TemplateUID: "template-uid", Image: image, WorkerPool: "pool-a", WorkerPod: "pod", WorkerPodUID: "pod-uid",
				SandboxClass: "SANDBOX_CLASS_GVISOR", BootstrapPublicKey: "synthetic-key",
				SnapshotOnPause: "SNAPSHOT_CONTENT_SCOPE_DATA", SnapshotOnCommit: "SNAPSHOT_CONTENT_SCOPE_DATA",
				ResumeFromData: "RESUME_SOURCE_GOLDEN", SnapshotStorage: "gs://snapshots/test/"}}}
	}

	first := reserve("normal")
	entered, release, next := make(chan struct{}), make(chan struct{}), make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithAttemptDispatchLock(ctx, first, func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- store.WithAttemptDispatchLock(ctx, first, func(context.Context) error {
			close(next)
			return nil
		})
	}()
	select {
	case <-next:
		t.Fatal("same attempt dispatched concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-next:
	case <-time.After(2 * time.Second):
		t.Fatal("attempt dispatch lock was not released")
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	ax := &fakeAX{write: true}
	bridge := makeBridge(first, ax)
	if _, err := bridge.Launch(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, state, _, err := store.CurrentAttempt(ctx, first); err != nil || state != "running" {
		t.Fatalf("normal launch: %q %v", state, err)
	}
	if err := store.RequestCancel(ctx, org, first.RunID); err != nil {
		t.Fatal(err)
	}
	if err := bridge.StopKnown(ctx, first); !errors.Is(err, workflow.ErrFenced) {
		t.Fatalf("missing owner revocation was accepted: %v", err)
	}
	bridge.RevokeOwner = func(context.Context, workflow.Attempt) error { return nil }
	if err := bridge.StopKnown(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(ctx, first, true, strings.Repeat("f", 64)); !errors.Is(err, workflow.ErrFenced) {
		t.Fatalf("stopped owner published: %v", err)
	}

	uncertain := reserve("uncertain-written")
	ax = &fakeAX{write: true, applyErr: errors.New("lost acknowledgement")}
	bridge = makeBridge(uncertain, ax)
	if _, err := bridge.Launch(ctx, uncertain); err == nil {
		t.Fatal("lost acknowledgement accepted as successful launch")
	}
	if _, state, _, err := store.CurrentAttempt(ctx, uncertain); err != nil || state != "reconciling" {
		t.Fatalf("uncertain launch not fenced: %q %v", state, err)
	}
	if _, err := bridge.ReconcileUnknown(ctx, uncertain); err != nil {
		t.Fatal(err)
	}
	if _, state, _, err := store.CurrentAttempt(ctx, uncertain); err != nil || state != "running" {
		t.Fatalf("read-back did not recover owner: %q %v", state, err)
	}

	missing := reserve("uncertain-missing")
	ax = &fakeAX{applyErr: errors.New("no write")}
	bridge = makeBridge(missing, ax)
	if _, err := bridge.Launch(ctx, missing); err == nil {
		t.Fatal("failed write accepted")
	}
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := bridge.ReconcileUnknown(short, missing); !errors.Is(err, ErrPending) {
		t.Fatalf("missing task treated as authoritative stop: %v", err)
	}
	if _, state, _, err := store.CurrentAttempt(ctx, missing); err != nil || state != "reconciling" {
		t.Fatalf("missing task unfenced: %q %v", state, err)
	}
}

func bridgePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	admin, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "bridge_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}
