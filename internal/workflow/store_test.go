package workflow

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
)

func TestWorkflowPostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	org, other := organization(t, pool, "a"), organization(t, pool, "b")
	project, err := store.CreateProject(ctx, org, "project-a", "Project A")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProject(ctx, other, "project-a", "Project B"); err != nil {
		t.Fatal(err)
	}
	in := RunInput{org, project, "request-1", strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := store.CreateRun(ctx, in)
	if err != nil || replay.ID != run.ID {
		t.Fatalf("launch replay: %+v, %v", replay, err)
	}
	changed := in
	changed.BundleSHA256 = strings.Repeat("d", 64)
	if _, err := store.CreateRun(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed frozen replay accepted: %v", err)
	}
	if _, err := store.GetRun(ctx, other, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant escaped run lookup: %v", err)
	}
	if _, err := store.FinalizeRun(ctx, other, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant escaped finalization: %v", err)
	}
	emptyInput := in
	emptyInput.LaunchKey = "empty-graph"
	empty, err := store.CreateRun(ctx, emptyInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET state='active' WHERE organization_id=$1 AND id=$2`, org, empty.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeRun(ctx, org, empty.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("empty graph finalized: %v", err)
	}
	if _, err := store.AddTask(ctx, other, run.ID, "implement", in.BundleSHA256, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("tenant escaped task creation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_tasks
		(organization_id,run_id,task_key,input_sha256,max_attempts)
		VALUES ($1,$2,'cross_tenant',$3,1)`, other, run.ID, in.BundleSHA256); err == nil {
		t.Fatal("cross-tenant task reference accepted by database")
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET bundle_sha256=$1 WHERE organization_id=$2 AND id=$3`, changed.BundleSHA256, org, run.ID); err == nil {
		t.Fatal("frozen run mutated")
	}
	task, err := store.AddTask(ctx, org, run.ID, "implement", in.BundleSHA256, 2)
	if err != nil {
		t.Fatal(err)
	}
	if again, err := store.AddTask(ctx, org, run.ID, "implement", in.BundleSHA256, 2); err != nil || again != task {
		t.Fatalf("task replay: %q, %v", again, err)
	}
	if _, err := store.AddTask(ctx, org, run.ID, "implement", in.BundleSHA256, 3); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed task budget accepted: %v", err)
	}
	if _, err := store.ReserveAttempt(ctx, org, run.ID, task); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsealed graph dispatched: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTask(ctx, org, run.ID, "late", in.BundleSHA256, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("sealed graph changed: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan struct {
		a   Attempt
		err error
	}, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, err := store.ReserveAttempt(ctx, org, run.ID, task)
			results <- struct {
				a   Attempt
				err error
			}{a, err}
		}()
	}
	wg.Wait()
	close(results)
	var first Attempt
	winners, conflicts := 0, 0
	for result := range results {
		if result.err == nil {
			first = result.a
			winners++
		} else if errors.Is(result.err, ErrConflict) {
			conflicts++
		} else {
			t.Fatal(result.err)
		}
	}
	if winners != 1 || conflicts != 11 || first.OwnerGeneration != 1 {
		t.Fatalf("reservation race: %d winners, %d conflicts, generation %d", winners, conflicts, first.OwnerGeneration)
	}
	unresolved, err := store.ListUnresolvedAttempts(ctx, "", "", 100)
	if err != nil || len(unresolved) != 1 || unresolved[0].ID != first.ID || unresolved[0].State != "reserved" {
		t.Fatalf("reserved owner missing from recovery scan: %+v, %v", unresolved, err)
	}
	if _, err := store.AddTask(ctx, org, run.ID, "late", in.BundleSHA256, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed active graph accepted: %v", err)
	}
	if err := store.ConfirmStarted(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, first); err != nil {
		t.Fatalf("duplicate start: %v", err)
	}
	if err := store.MarkUnknown(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeRun(ctx, org, run.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unfinished task graph finalized: %v", err)
	}
	if err := store.FinishAttempt(ctx, first, true, in.BundleSHA256); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncertain result accepted: %v", err)
	}
	if _, err := store.ReserveAttempt(ctx, org, run.ID, task); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncertain launch replaced: %v", err)
	}
	if err := store.ConfirmStopped(ctx, first); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStopped(ctx, first); err != nil {
		t.Fatalf("duplicate stop: %v", err)
	}
	if err := store.FinishAttempt(ctx, first, true, in.BundleSHA256); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale result accepted: %v", err)
	}
	second, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil || second.OwnerGeneration != 2 || second.ID == first.ID || second.FenceToken == first.FenceToken {
		t.Fatalf("replacement fence: %+v, %v", second, err)
	}
	if err := store.ConfirmStarted(ctx, second); err != nil {
		t.Fatal(err)
	}
	bad := second
	bad.FenceToken = first.FenceToken
	if err := store.FinishAttempt(ctx, bad, true, in.BundleSHA256); !errors.Is(err, ErrFenced) {
		t.Fatalf("wrong owner accepted: %v", err)
	}
	finishErrors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			finishErrors <- store.FinishAttempt(ctx, second, true, in.BundleSHA256)
		}()
	}
	wg.Wait()
	close(finishErrors)
	for err := range finishErrors {
		if err != nil {
			t.Fatalf("concurrent duplicate result: %v", err)
		}
	}
	if _, err := store.ReserveAttempt(ctx, org, run.ID, task); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed task restarted: %v", err)
	}
	if state, err := store.FinalizeRun(ctx, org, run.ID); err != nil || state != "succeeded" {
		t.Fatalf("successful run finalization: %q, %v", state, err)
	}
	if state, err := store.FinalizeRun(ctx, org, run.ID); err != nil || state != "succeeded" {
		t.Fatalf("duplicate run finalization: %q, %v", state, err)
	}
	if err := store.RequestCancel(ctx, org, run.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("terminal run cancelled: %v", err)
	}

	oneShot, err := store.AddTask(ctx, org, run.ID, "late", in.BundleSHA256, 1)
	if !errors.Is(err, ErrConflict) || oneShot != "" {
		t.Fatalf("active graph mutation: %q, %v", oneShot, err)
	}
	events, err := store.EventsAfter(ctx, org, run.ID, 0, 100)
	if err != nil || len(events) != 10 {
		t.Fatalf("events: %d, %v", len(events), err)
	}
	for i, e := range events {
		if e.ID != int64(i+1) {
			t.Fatalf("noncontiguous run cursor at %d: %d", i, e.ID)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workflow_events WHERE organization_id=$1 AND run_id=$2 AND id=1`, org, run.ID); err == nil {
		t.Fatal("event deletion accepted")
	}
	if next, err := store.EventsAfter(ctx, org, run.ID, events[3].ID, 100); err != nil || len(next) != len(events)-4 {
		t.Fatalf("event cursor: %d, %v", len(next), err)
	}
	if cross, err := store.EventsAfter(ctx, other, run.ID, 0, 100); err != nil || len(cross) != 0 {
		t.Fatalf("tenant escaped events: %d, %v", len(cross), err)
	}
}

func TestWorkflowBudgetAndCancellationPostgres(t *testing.T) {
	pool := testPool(t)
	store, _ := New(pool)
	ctx := t.Context()
	org := organization(t, pool, "c")
	project, err := store.CreateProject(ctx, org, "project-c", "Project C")
	if err != nil {
		t.Fatal(err)
	}
	in := RunInput{org, project, "request-2", strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "verify", in.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := store.AddTask(ctx, org, run.ID, "document", in.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	a, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(ctx, a, false, in.BundleSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveAttempt(ctx, org, run.ID, task); !errors.Is(err, ErrConflict) {
		t.Fatalf("budget exhausted but retried: %v", err)
	}
	if _, err := store.FinalizeRun(ctx, org, run.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("pending sibling ignored: %v", err)
	}
	remainingAttempt, err := store.ReserveAttempt(ctx, org, run.ID, remaining)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, remainingAttempt); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(ctx, remainingAttempt, true, in.BundleSHA256); err != nil {
		t.Fatal(err)
	}
	if state, err := store.FinalizeRun(ctx, org, run.ID); err != nil || state != "failed" {
		t.Fatalf("blocked run finalization: %q, %v", state, err)
	}
	if state, err := store.FinalizeRun(ctx, org, run.ID); err != nil || state != "failed" {
		t.Fatalf("duplicate failed finalization: %q, %v", state, err)
	}
	if err := store.RequestCancel(ctx, org, run.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("failed run cancelled: %v", err)
	}
	active := in
	active.LaunchKey = "request-3"
	activeRun, err := store.CreateRun(ctx, active)
	if err != nil {
		t.Fatal(err)
	}
	activeTask, err := store.AddTask(ctx, org, activeRun.ID, "implement", in.BundleSHA256, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, activeRun.ID); err != nil {
		t.Fatal(err)
	}
	owner, err := store.ReserveAttempt(ctx, org, activeRun.ID, activeTask)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestCancel(ctx, org, activeRun.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinalizeRun(ctx, org, activeRun.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancellation raced finalization: %v", err)
	}
	if err := store.FinalizeCancel(ctx, org, activeRun.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("active work cancelled without termination: %v", err)
	}
	if err := store.FinishAttempt(ctx, owner, true, in.BundleSHA256); !errors.Is(err, ErrFenced) {
		t.Fatalf("cancelled owner published: %v", err)
	}
	if err := store.ConfirmStopped(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeCancel(ctx, org, activeRun.ID); err != nil {
		t.Fatal(err)
	}
	raceInput := in
	raceInput.LaunchKey = "request-race"
	raceRun, err := store.CreateRun(ctx, raceInput)
	if err != nil {
		t.Fatal(err)
	}
	raceTask, err := store.AddTask(ctx, org, raceRun.ID, "review", in.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, raceRun.ID); err != nil {
		t.Fatal(err)
	}
	raceOwner, err := store.ReserveAttempt(ctx, org, raceRun.ID, raceTask)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, raceOwner); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(ctx, raceOwner, true, in.BundleSHA256); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	final := make(chan error, 1)
	cancel := make(chan error, 1)
	go func() {
		<-start
		_, err := store.FinalizeRun(ctx, org, raceRun.ID)
		final <- err
	}()
	go func() {
		<-start
		cancel <- store.RequestCancel(ctx, org, raceRun.ID)
	}()
	close(start)
	finalErr, cancelErr := <-final, <-cancel
	if (finalErr == nil) == (cancelErr == nil) ||
		(finalErr != nil && !errors.Is(finalErr, ErrConflict)) ||
		(cancelErr != nil && !errors.Is(cancelErr, ErrConflict)) {
		t.Fatalf("finalize/cancel race: final=%v cancel=%v", finalErr, cancelErr)
	}
	actual, err := store.GetRun(ctx, org, raceRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalErr == nil && actual.State != "succeeded" || cancelErr == nil && actual.State != "cancel_requested" {
		t.Fatalf("race produced inconsistent state: %q", actual.State)
	}
}

func TestDependencyReservationGatePostgres(t *testing.T) {
	pool := testPool(t)
	store, _ := New(pool)
	ctx := t.Context()
	org := organization(t, pool, "dependency")
	project, err := store.CreateProject(ctx, org, "project-dependency", "Dependency")
	if err != nil {
		t.Fatal(err)
	}
	in := RunInput{org, project, "dependency-run", strings.Repeat("a", 40), strings.Repeat("b", 64), strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := store.AddTask(ctx, org, run.ID, "plan", in.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	build, err := store.AddTask(ctx, org, run.ID, "build", in.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_task_dependencies
		(organization_id,run_id,task_id,depends_on_task_id) VALUES ($1,$2,$3,$4)`, org, run.ID, build, plan); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveAttempt(ctx, org, run.ID, build); !errors.Is(err, ErrConflict) {
		t.Fatalf("dependent task dispatched before parent completion: %v", err)
	}
	denied := errors.New("binding denied")
	if _, err := store.ReserveAttemptWithBinding(ctx, org, run.ID, plan,
		func(context.Context, pgx.Tx, Attempt) error { return denied }); !errors.Is(err, denied) {
		t.Fatalf("binding failure did not fail reservation: %v", err)
	}
	a, err := store.ReserveAttempt(ctx, org, run.ID, plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(ctx, a, true, in.BundleSHA256); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveAttempt(ctx, org, run.ID, build); err != nil {
		t.Fatalf("dependent task remained blocked after parent success: %v", err)
	}
}

func testPool(t *testing.T) *pgxpool.Pool {
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
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_workflow_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
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
	if err := pool.QueryRow(t.Context(), `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),$1,$2) RETURNING id`, "org-"+suffix, "Organization "+suffix).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
