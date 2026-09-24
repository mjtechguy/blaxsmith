package interact

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// Watcher runs `bx watch` for one attempt at a time, persists what the guest
// streams, and delivers persisted answers and steers through guest exec.
type Watcher struct {
	Store *Store
	Guest GuestExec
	Poll  time.Duration // delivery poll; zero selects 2s
}

// Run watches until ctx ends (the owner cancels it when the attempt stops),
// reconnecting with bounded backoff. Each reconnect redelivers first.
func (w *Watcher) Run(ctx context.Context, a workflow.Attempt) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		if err := w.session(ctx, a); err != nil && ctx.Err() == nil {
			log.Printf("interaction watch for attempt %s: %v", a.ID, err)
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func (w *Watcher) session(ctx context.Context, a workflow.Attempt) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w.Deliver(ctx, a)
	cursor, err := w.Store.Cursor(ctx, a)
	if err != nil {
		return err
	}
	proc, err := w.Guest.Start(ctx, a, []string{"bx", "watch", "--cursor", strconv.FormatInt(cursor, 10)}, false)
	if err != nil {
		return err
	}
	wake, unsubscribe := w.Store.subscribe(a.ID)
	defer unsubscribe()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		poll := w.Poll
		if poll == 0 {
			poll = 2 * time.Second
		}
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			case <-ticker.C:
			}
			w.Deliver(ctx, a)
		}
	}()
	defer wg.Wait()
	scanner := bufio.NewScanner(proc.Stdout)
	scanner.Buffer(make([]byte, 64<<10), 256<<10)
	for scanner.Scan() {
		if err := w.Store.Persist(ctx, a, scanner.Bytes()); errors.Is(err, workflow.ErrInvalid) {
			log.Printf("interaction watch for attempt %s: skipped malformed line", a.ID)
		} else if err != nil {
			cancel()
			return err
		}
	}
	scanErr := scanner.Err()
	cancel()
	if _, err := proc.Wait(); err != nil && scanErr == nil {
		scanErr = err
	}
	return scanErr
}

// Deliver sends every pending answer, then every pending steer, in order. It
// stops at the first failure so ordering holds; the next call retries.
func (w *Watcher) Deliver(ctx context.Context, a workflow.Attempt) {
	answers, steers, err := w.Store.Pending(ctx, a)
	if err != nil {
		return
	}
	for _, d := range answers {
		code, err := w.exec(ctx, a, d.Argv)
		// Exit 2 means the guest no longer knows the id; retrying cannot help.
		if err != nil || (code != 0 && code != 2) {
			return
		}
		if w.Store.MarkAnswerDelivered(ctx, a.OrganizationID, d.ID) != nil {
			return
		}
	}
	for _, d := range steers {
		code, err := w.exec(ctx, a, d.Argv)
		if err != nil || code != 0 {
			return
		}
		if w.Store.MarkSteerDelivered(ctx, a.OrganizationID, d.ID) != nil {
			return
		}
	}
}

func (w *Watcher) exec(ctx context.Context, a workflow.Attempt, argv []string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	proc, err := w.Guest.Start(ctx, a, argv, false)
	if err != nil {
		return -1, err
	}
	_, _ = io.Copy(io.Discard, proc.Stdout)
	return proc.Wait()
}
