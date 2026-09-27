package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const dispatchLeaderLock = `SELECT pg_try_advisory_lock(hashtextextended('blaxsmith/dispatch-leader/v1', 0))`

// runDispatchCoordinator keeps launch, completion, recovery, and stage
// progress polling single-leader across app replicas. The advisory lock
// belongs to this acquired PostgreSQL session and releases if the process or
// its database connection dies.
func runDispatchCoordinator(ctx context.Context, pool *pgxpool.Pool, product *productDispatch) {
	if pool == nil || product == nil || product.Dispatcher == nil {
		return
	}
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("dispatch leader connection unavailable: %v", err)
			}
			if !waitContext(ctx, 3*time.Second) {
				return
			}
			continue
		}
		var leader bool
		err = conn.QueryRow(ctx, dispatchLeaderLock).Scan(&leader)
		if err == nil && leader {
			runDispatchLeader(ctx, conn, product)
			unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			var unlocked bool
			if unlockErr := conn.QueryRow(unlockCtx,
				`SELECT pg_advisory_unlock(hashtextextended('blaxsmith/dispatch-leader/v1', 0))`).Scan(&unlocked); unlockErr != nil || !unlocked {
				log.Printf("dispatch leader lock release failed: %v", unlockErr)
			}
			cancel()
		} else if err != nil && ctx.Err() == nil {
			log.Printf("dispatch leader lock unavailable: %v", err)
		}
		conn.Release()
		if !waitContext(ctx, 3*time.Second) {
			return
		}
	}
}

func runDispatchLeader(ctx context.Context, connection *pgxpool.Conn, product *productDispatch) {
	var afterOrganizationID string
	var cancellation [2]string
	var completion, recovery, progress [2]string // Keyset cursors; empty restarts a pass.
	var stopCancelled func(context.Context)
	if product.Completion != nil {
		stopCancelled = func(ctx context.Context) {
			done, err := product.Completion.SweepCancelled(ctx, cancellation[0], cancellation[1], 10)
			logSweep(ctx, "cancellation", err)
			cancellation = [2]string{done.AfterOrganizationID, done.AfterAttemptID}
		}
	}
	var advanceRuns func(context.Context)
	if product.Workflow != nil {
		advanceRuns = func(ctx context.Context) {
			done, err := product.Workflow.Progress(ctx, progress[0], progress[1], 10, product.Escalations)
			logSweep(ctx, "stage progress", err)
			progress = [2]string{done.AfterOrganizationID, done.AfterRunID}
		}
	}
	superviseDispatch(ctx, func(ctx context.Context) error {
		return connection.QueryRow(ctx, `SELECT 1`).Scan(new(int))
	}, func(ctx context.Context) {
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		batch, err := product.Dispatcher.DispatchBatch(batchCtx, afterOrganizationID, 1, 1)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("dispatch batch unavailable: %v", err)
		}
		if err == nil {
			afterOrganizationID = batch.NextOrganizationID
		}
		if product.Completion != nil {
			sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			done, err := product.Completion.Sweep(sweepCtx, completion[0], completion[1], 10)
			cancel()
			logSweep(ctx, "completion", err)
			completion = [2]string{done.AfterOrganizationID, done.AfterAttemptID}
		}
		if product.Recovery != nil {
			sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
			done, err := product.Recovery.Sweep(sweepCtx, recovery[0], recovery[1], 10)
			cancel()
			logSweep(ctx, "recovery", err)
			recovery = [2]string{done.AfterOrganizationID, done.AfterAttemptID}
		}
	}, product.RenewLeases, stopCancelled, advanceRuns)
}

// Keep authority checks and lease renewal responsive while a bounded dispatch
// pass waits on a guest. Launch and recovery stay serial: recovery must not
// mistake an in-flight launch for a crashed dispatcher. All goroutines finish
// before the caller releases the leader connection.
func superviseDispatch(ctx context.Context, heartbeat func(context.Context) error, work func(context.Context), maintenance ...func(context.Context)) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	workers.Go(func() {
		for ctx.Err() == nil {
			probe, stop := context.WithTimeout(ctx, 3*time.Second)
			err := heartbeat(probe)
			stop()
			if err != nil {
				if ctx.Err() == nil {
					log.Printf("dispatch leader lost database session: %v", err)
				}
				cancel()
				return
			}
			if !waitContext(ctx, 2*time.Second) {
				return
			}
		}
	})
	for _, tickWork := range maintenance {
		if tickWork == nil {
			continue
		}
		workers.Go(func() {
			for ctx.Err() == nil {
				tick, stop := context.WithTimeout(ctx, 30*time.Second)
				tickWork(tick)
				stop()
				if !waitContext(ctx, 2*time.Second) {
					return
				}
			}
		})
	}
	for ctx.Err() == nil {
		work(ctx)
		if !waitContext(ctx, 2*time.Second) {
			return
		}
	}
}

// logSweep reports a bounded pass failure. The pass cursor still advances, so
// one failing attempt or run cannot starve the rest.
func logSweep(ctx context.Context, name string, err error) {
	if err != nil && ctx.Err() == nil {
		log.Printf("%s sweep: %v", name, err)
	}
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
