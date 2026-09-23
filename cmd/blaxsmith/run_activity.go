package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const activityBatch = 100
const activityStreamLimit = 128
const activityReplayLimit = 500

// PostgreSQL notifications wake readers; workflow_events is the durable log.
// One LISTEN connection serves this process, while the stream cap bounds
// periodic revalidation and catch-up queries.
type activityHub struct {
	pool    *pgxpool.Pool
	mu      sync.Mutex
	waiters map[string]map[chan struct{}]struct{}
	slots   chan struct{}
	ready   chan struct{}
	once    sync.Once
}

func newActivityHub(pool *pgxpool.Pool) *activityHub {
	return &activityHub{pool: pool, waiters: make(map[string]map[chan struct{}]struct{}), slots: make(chan struct{}, activityStreamLimit), ready: make(chan struct{})}
}

func (h *activityHub) enter() (func(), bool) {
	select {
	case h.slots <- struct{}{}:
		return func() { <-h.slots }, true
	default:
		return nil, false
	}
}

func (h *activityHub) subscribe(key string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.waiters[key] == nil {
		h.waiters[key] = make(map[chan struct{}]struct{})
	}
	h.waiters[key][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.waiters[key], ch)
		if len(h.waiters[key]) == 0 {
			delete(h.waiters, key)
		}
		h.mu.Unlock()
	}
}

func (h *activityHub) wake(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.waiters[key] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *activityHub) listen(ctx context.Context) {
	for ctx.Err() == nil {
		conn, err := h.pool.Acquire(ctx)
		if err == nil {
			_, err = conn.Exec(ctx, `LISTEN blaxsmith_workflow_events`)
			if err == nil {
				h.once.Do(func() { close(h.ready) })
				for ctx.Err() == nil {
					got, waitErr := conn.Conn().WaitForNotification(ctx)
					if waitErr != nil {
						err = waitErr
						break
					}
					h.wake(got.Payload)
				}
			}
			conn.Release()
		}
		if ctx.Err() != nil {
			return
		}
		log.Printf("run activity listener reconnecting: %v", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

type runActivityHandler struct {
	guard *identity.BrowserGuard
	store *workflow.Store
	hub   *activityHub
}

func (h *runActivityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if h.hub == nil {
		http.Error(w, "activity unavailable", http.StatusServiceUnavailable)
		return
	}
	leave, ok := h.hub.enter()
	if !ok {
		http.Error(w, "too many activity streams", http.StatusServiceUnavailable)
		return
	}
	defer leave()
	caller, err := h.guard.StreamCaller(r.Context(), r.Header)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	after, err := activityCursor(r)
	if err != nil {
		http.Error(w, "invalid event cursor", http.StatusBadRequest)
		return
	}
	runID := r.PathValue("runID")
	head, err := h.store.EventHead(r.Context(), caller.OrganizationID, runID)
	if errors.Is(err, workflow.ErrNotFound) || errors.Is(err, workflow.ErrInvalid) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "activity unavailable", http.StatusServiceUnavailable)
		return
	}
	if after > head {
		http.Error(w, "invalid event cursor", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "stream unsupported", http.StatusInternalServerError)
		return
	}
	wake, unsubscribe := h.hub.subscribe(caller.OrganizationID + ":" + runID)
	defer unsubscribe()
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if !writeActivity(w, flusher, "retry: 5000\n\n") {
		return
	}
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		count, ok := h.replay(w, flusher, r.Context(), r.Header, caller, runID, &after)
		if !ok || count == activityReplayLimit {
			return
		}
		heartbeat := false
		select {
		case <-r.Context().Done():
			return
		case <-wake:
		case <-ticker.C:
			heartbeat = true
		}
		if !h.sameCaller(r.Context(), r.Header, caller) {
			return
		}
		if heartbeat && !writeActivity(w, flusher, ": heartbeat\n\n") {
			return
		}
	}
}

func activityCursor(r *http.Request) (int64, error) {
	values := r.URL.Query()["after"]
	if len(values) > 1 {
		return 0, errors.New("duplicate cursor")
	}
	raw := "0"
	if len(values) == 1 {
		raw = values[0]
	}
	if headers := r.Header.Values("Last-Event-ID"); len(headers) > 0 {
		if len(headers) != 1 {
			return 0, errors.New("duplicate event id")
		}
		raw = headers[0]
	}
	if raw == "" || len(raw) > 19 || strings.Trim(raw, "0123456789") != "" {
		return 0, errors.New("invalid cursor")
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("invalid cursor")
	}
	return n, nil
}

func (h *runActivityHandler) sameCaller(ctx context.Context, header http.Header, caller identity.Caller) bool {
	current, err := h.guard.StreamCaller(ctx, header)
	return err == nil && current.OrganizationID == caller.OrganizationID && current.PrincipalID == caller.PrincipalID &&
		current.SessionID == caller.SessionID && current.Role == caller.Role
}

func (h *runActivityHandler) replay(w http.ResponseWriter, flusher http.Flusher, ctx context.Context, header http.Header, caller identity.Caller, runID string, after *int64) (int, bool) {
	count := 0
	for count < activityReplayLimit {
		if count > 0 && !h.sameCaller(ctx, header, caller) {
			return count, false
		}
		events, err := h.store.EventsAfter(ctx, caller.OrganizationID, runID, *after, activityBatch)
		if err != nil {
			return count, false
		}
		for _, event := range events {
			payload, err := json.Marshal(struct {
				ID         string  `json:"id"`
				RunID      string  `json:"runId"`
				TaskID     *string `json:"taskId,omitempty"`
				AttemptID  *string `json:"attemptId,omitempty"`
				Kind       string  `json:"kind"`
				OccurredAt string  `json:"occurredAt"`
			}{strconv.FormatInt(event.ID, 10), event.RunID, event.TaskID, event.AttemptID,
				event.Kind, event.OccurredAt.UTC().Format(time.RFC3339Nano)})
			if err != nil || !writeActivity(w, flusher, fmt.Sprintf("id: %d\ndata: %s\n\n", event.ID, payload)) {
				return count, false
			}
			*after = event.ID
			count++
		}
		if len(events) < activityBatch {
			break
		}
	}
	return count, true
}

func writeActivity(w http.ResponseWriter, flusher http.Flusher, message string) bool {
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return false
	}
	if _, err := w.Write([]byte(message)); err != nil {
		return false
	}
	flusher.Flush()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Time{}); err != nil {
		return false
	}
	return true
}
