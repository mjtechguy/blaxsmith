package main

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/dispatch"
)

const dispatchLeaderLock = `SELECT pg_try_advisory_lock(hashtextextended('blaxsmith/dispatch-leader/v1', 0))`

// runDispatchCoordinator keeps launch polling single-leader across app replicas.
// The advisory lock belongs to this acquired PostgreSQL session and releases if
// the process or its database connection dies.
func runDispatchCoordinator(ctx context.Context, pool *pgxpool.Pool, dispatcher *dispatch.Dispatcher) {
	if pool == nil || dispatcher == nil {
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
			runDispatchLeader(ctx, conn, dispatcher)
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

func runDispatchLeader(ctx context.Context, connection *pgxpool.Conn, dispatcher *dispatch.Dispatcher) {
	var afterOrganizationID string
	for ctx.Err() == nil {
		if err := connection.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
			log.Printf("dispatch leader lost database session: %v", err)
			return
		}
		batchCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		batch, err := dispatcher.DispatchBatch(batchCtx, afterOrganizationID, 1, 1)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("dispatch batch unavailable: %v", err)
		}
		if err == nil {
			afterOrganizationID = batch.NextOrganizationID
		}
		if !waitContext(ctx, 2*time.Second) {
			return
		}
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
