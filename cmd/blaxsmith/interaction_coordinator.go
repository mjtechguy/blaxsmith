package main

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const interactionLeaderKey = "blaxsmith/interaction-leader/v1"
const maxInteractionWatchers = 256

// runInteractionCoordinator keeps one watcher per running attempt on a single
// leader replica, stops a watcher when its attempt stops, and redelivers
// platform escalation answers to the engine callback.
func runInteractionCoordinator(ctx context.Context, pool *pgxpool.Pool, store *workflow.Store, watcher *interact.Watcher) {
	if pool == nil || store == nil || watcher == nil || watcher.Store == nil {
		return
	}
	for ctx.Err() == nil {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			if !waitContext(ctx, 3*time.Second) {
				return
			}
			continue
		}
		var leader bool
		err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, interactionLeaderKey).Scan(&leader)
		if err == nil && leader {
			runInteractionLeader(ctx, conn, store, watcher)
			unlockCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, interactionLeaderKey)
			cancel()
		} else if err != nil && ctx.Err() == nil {
			log.Printf("interaction leader lock unavailable: %v", err)
		}
		conn.Release()
		if !waitContext(ctx, 3*time.Second) {
			return
		}
	}
}

func runInteractionLeader(ctx context.Context, conn *pgxpool.Conn, store *workflow.Store, watcher *interact.Watcher) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	active := map[string]context.CancelFunc{}
	attempts := map[string]workflow.Attempt{}
	defer func() { cancel(); wg.Wait() }()
	for ctx.Err() == nil {
		if err := conn.QueryRow(ctx, `SELECT 1`).Scan(new(int)); err != nil {
			log.Printf("interaction leader lost database session: %v", err)
			return
		}
		running, err := listRunning(ctx, store)
		if err == nil {
			for id, stop := range active {
				if _, ok := running[id]; !ok {
					stop()
					delete(active, id)
					if err := watcher.Store.CancelOpen(ctx, attempts[id]); err != nil {
						log.Printf("close interactions for stopped attempt %s: %v", id, err)
					}
					delete(attempts, id)
				}
			}
			for id, attempt := range running {
				if _, ok := active[id]; ok || watcher.Guest == nil || len(active) >= maxInteractionWatchers {
					continue
				}
				watchCtx, stop := context.WithCancel(ctx)
				active[id], attempts[id] = stop, attempt
				wg.Add(1)
				go func() { defer wg.Done(); watcher.Run(watchCtx, attempt) }()
			}
		} else if ctx.Err() == nil {
			log.Printf("interaction attempts unavailable: %v", err)
		}
		watcher.Store.DeliverEscalations(ctx)
		if !waitContext(ctx, 2*time.Second) {
			return
		}
	}
}

func listRunning(ctx context.Context, store *workflow.Store) (map[string]workflow.Attempt, error) {
	out := map[string]workflow.Attempt{}
	afterOrg, afterAttempt := "", ""
	for len(out) < maxInteractionWatchers*2 {
		page, err := store.ListRunningAttempts(ctx, afterOrg, afterAttempt, 100)
		if err != nil {
			return nil, err
		}
		for _, attempt := range page {
			out[attempt.ID] = attempt
		}
		if len(page) < 100 {
			break
		}
		afterOrg, afterAttempt = page[len(page)-1].OrganizationID, page[len(page)-1].ID
	}
	return out, nil
}

func interactionStore(pool *pgxpool.Pool) *workflow.Store {
	store, _ := workflow.New(pool)
	return store
}

// interactionWatcher runs guest `bx` commands over the shared guest router
// (BLAXSMITH_GUEST_ROUTER, one connection with terminals). Without it,
// platform escalations still work and guest watchers stay off.
func interactionWatcher(store *interact.Store, guests *terminal.Router) *interact.Watcher {
	watcher := &interact.Watcher{Store: store}
	if guests != nil {
		watcher.Guest = &interact.AteGuest{Router: guests}
	}
	return watcher
}

// escalationSink adapts the flow engine's escalations onto platform
// interactions. The escalation Key is the interaction's guest id, so a retried
// Raise finds the same row.
type escalationSink struct{ store *interact.Store }

func (e escalationSink) Raise(ctx context.Context, esc workflow.Escalation) error {
	options := make([]interact.Option, 0, len(esc.Options))
	for _, o := range esc.Options {
		options = append(options, interact.Option{ID: o.ID, Label: o.Label, Description: o.Description, Recommended: o.Recommended})
	}
	_, err := e.store.Raise(ctx, esc.OrganizationID, esc.RunID, esc.StageKey, interact.Interaction{
		ID: esc.Key, Kind: "escalation", Title: esc.Title, BodyMD: esc.BodyMD, Options: options, Blocking: true})
	return err
}

// escalationAnswer applies an answered platform escalation to the engine.
// The answer's InteractionID is the escalation key; the callback's
// interactionID argument is the row UUID and is not used.
func escalationAnswer(store *workflow.Store) interact.EscalationAnswerFunc {
	return func(ctx context.Context, orgID, runID, _, _ string, a interact.Answer) error {
		if len(a.OptionIDs) != 1 {
			return nil // not an engine action; nothing to apply
		}
		action := a.OptionIDs[0]
		err := store.ResolveEscalation(ctx, orgID, runID, a.InteractionID, action, 1)
		if errors.Is(err, workflow.ErrInvalid) || errors.Is(err, workflow.ErrNotFound) || errors.Is(err, workflow.ErrConflict) {
			log.Printf("escalation %s answer %q not applied: %v", a.InteractionID, action, err)
			return nil // permanent; redelivery cannot fix it
		}
		return err
	}
}
