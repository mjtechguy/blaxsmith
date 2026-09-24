package main

import (
	"context"
	"log"
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
	var completion, recovery, progress [2]string // Keyset cursors; empty restarts a pass.
	for ctx.Err() == nil {
		if err := connection.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
			log.Printf("dispatch leader lost database session: %v", err)
			return
		}
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		batch, err := product.Dispatcher.DispatchBatch(batchCtx, afterOrganizationID, 1, 1)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("dispatch batch unavailable: %v", err)
		}
		if err == nil {
			afterOrganizationID = batch.NextOrganizationID
		}
		sweepCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		if product.Completion != nil {
			done, err := product.Completion.Sweep(sweepCtx, completion[0], completion[1], 10)
			logSweep(ctx, "completion", err)
			completion = [2]string{done.AfterOrganizationID, done.AfterAttemptID}
		}
		if product.Recovery != nil {
			done, err := product.Recovery.Sweep(sweepCtx, recovery[0], recovery[1], 10)
			logSweep(ctx, "recovery", err)
			recovery = [2]string{done.AfterOrganizationID, done.AfterAttemptID}
		}
		if product.Workflow != nil {
			done, err := product.Workflow.Progress(sweepCtx, progress[0], progress[1], 10, product.Escalations)
			logSweep(ctx, "stage progress", err)
			progress = [2]string{done.AfterOrganizationID, done.AfterRunID}
		}
		if product.RenewLeases != nil {
			product.RenewLeases(sweepCtx)
		}
		cancel()
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
