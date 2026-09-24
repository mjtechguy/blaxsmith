package terminal

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Hub tracks this process's terminal sockets per attempt so control changes
// reach them at once, and so only the newest socket of the holder session
// controls (a reconnect or second tab never makes a second controller).
//
// ponytail: in-process only. Other replicas notice holder changes on their
// periodic recheck; add a LISTEN channel when the app runs >1 replica.
type Hub struct {
	mu      sync.Mutex
	seq     uint64
	members map[string]map[*Member]struct{}
}

type Member struct {
	hub              *Hub
	attempt, session string
	seq              uint64
	wake             chan struct{}
}

func NewHub() *Hub { return &Hub{members: map[string]map[*Member]struct{}{}} }

func (h *Hub) Join(attempt, session string) *Member {
	h.mu.Lock()
	h.seq++
	m := &Member{hub: h, attempt: attempt, session: session, seq: h.seq, wake: make(chan struct{}, 1)}
	if h.members[attempt] == nil {
		h.members[attempt] = map[*Member]struct{}{}
	}
	h.members[attempt][m] = struct{}{}
	h.mu.Unlock()
	h.Notify(attempt)
	return m
}

func (m *Member) Leave() {
	h := m.hub
	h.mu.Lock()
	delete(h.members[m.attempt], m)
	if len(h.members[m.attempt]) == 0 {
		delete(h.members, m.attempt)
	}
	h.mu.Unlock()
	h.Notify(m.attempt)
}

func (h *Hub) Notify(attempt string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for m := range h.members[attempt] {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

func (m *Member) Wake() <-chan struct{} { return m.wake }

// Controls reports whether m is the newest socket of the holder session.
func (m *Member) Controls(holderSession string) bool {
	if holderSession == "" || m.session != holderSession {
		return false
	}
	h := m.hub
	h.mu.Lock()
	defer h.mu.Unlock()
	for other := range h.members[m.attempt] {
		if other.session == holderSession && other.seq > m.seq {
			return false
		}
	}
	return true
}

// State is the server → client control frame.
type State struct {
	Type          string  `json:"type"` // always "state"
	Control       string  `json:"control"`
	Holder        *string `json:"holder"`
	Stage         string  `json:"stage"`
	AttemptStatus string  `json:"attemptStatus"`
}

// Access is one authorization decision for a socket.
type Access struct {
	Control bool // this socket may type and resize
	State   State
}

// Session serves one browser socket. Check must revalidate the session
// cookie, tenant/attempt authorization, and holder on every call; an error
// closes the socket with a safe message.
type Session struct {
	Guest   *Guest
	Check   func(context.Context) (Access, error)
	Wake    <-chan struct{}
	Recheck time.Duration // default 10s
}

const (
	maxClientFrame = 64 << 10
	writeTimeout   = 10 * time.Second
)

type attachResult struct {
	exit *int
	err  error
}

func (s *Session) Serve(ctx context.Context, c *websocket.Conn) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer c.CloseNow()
	c.SetReadLimit(maxClientFrame)
	recheck := s.Recheck
	if recheck <= 0 {
		recheck = 10 * time.Second
	}
	access, err := s.Check(ctx)
	if err != nil {
		s.fail(ctx, c, "terminal access denied")
		return
	}
	if !s.text(ctx, c, access.State) {
		return
	}

	input := make(chan []byte, 16)
	resize := make(chan [2]int, 1)
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		defer cancel()
		for {
			kind, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				select {
				case input <- data:
				case <-ctx.Done():
					return
				}
				continue
			}
			var frame struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(data, &frame) != nil || frame.Type != "resize" {
				continue // unknown client text frames are ignored
			}
			select { // keep only the latest size
			case <-resize:
			default:
			}
			resize <- [2]int{frame.Cols, frame.Rows}
		}
	}()
	defer func() { cancel(); <-readDone }()

	ticker := time.NewTicker(recheck)
	defer ticker.Stop()
	for {
		att, err := s.Guest.Attach(ctx, access.Control)
		if err != nil {
			s.fail(ctx, c, "terminal unavailable")
			return
		}
		done := make(chan attachResult, 1)
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			for {
				data, exit, err := att.Recv()
				if err != nil || exit != nil {
					done <- attachResult{exit, err}
					return
				}
				wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
				err = c.Write(wctx, websocket.MessageBinary, data)
				wcancel()
				if err != nil {
					done <- attachResult{nil, err}
					return
				}
			}
		}()
		reattach, closeMsg := s.pump(ctx, c, att, &access, input, resize, done, ticker.C)
		att.Close()
		<-finished
		if reattach {
			continue
		}
		if closeMsg != nil {
			_ = c.Close(websocket.StatusNormalClosure, *closeMsg)
		}
		return
	}
}

// pump relays one attachment until it ends or control changes.
func (s *Session) pump(ctx context.Context, c *websocket.Conn, att *Attachment, access *Access,
	input <-chan []byte, resize <-chan [2]int, done <-chan attachResult, tick <-chan time.Time) (bool, *string) {
	for {
		recheck := false
		select {
		case <-ctx.Done():
			return false, nil
		case r := <-done:
			if r.exit != nil {
				if s.text(ctx, c, struct {
					Type string `json:"type"`
					Code int    `json:"code"`
				}{"exit", *r.exit}) {
					msg := "exited"
					return false, &msg
				}
				return false, nil
			}
			if ctx.Err() == nil {
				s.fail(ctx, c, "terminal disconnected")
			}
			return false, nil
		case data := <-input:
			if access.Control { // viewer bytes are dropped here, and tmux -r ignores them too
				if att.Write(data) != nil {
					s.fail(ctx, c, "terminal disconnected")
					return false, nil
				}
			}
		case size := <-resize:
			if access.Control {
				rctx, rcancel := context.WithTimeout(ctx, 5*time.Second)
				_ = s.Guest.Resize(rctx, size[0], size[1])
				rcancel()
			}
		case <-s.Wake:
			recheck = true
		case <-tick:
			recheck = true
		}
		if !recheck {
			continue
		}
		next, err := s.Check(ctx)
		if err != nil {
			s.fail(ctx, c, "terminal access revoked")
			return false, nil
		}
		changed := next.State.Control != access.State.Control || next.State.AttemptStatus != access.State.AttemptStatus ||
			(next.State.Holder == nil) != (access.State.Holder == nil) ||
			(next.State.Holder != nil && access.State.Holder != nil && *next.State.Holder != *access.State.Holder)
		mode := next.Control != access.Control
		*access = next
		if changed || mode {
			if !s.text(ctx, c, next.State) {
				return false, nil
			}
		}
		if mode {
			return true, nil
		}
	}
}

func (s *Session) text(ctx context.Context, c *websocket.Conn, v any) bool {
	data, err := json.Marshal(v)
	if err != nil {
		return false
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, data) == nil
}

func (s *Session) fail(ctx context.Context, c *websocket.Conn, message string) {
	s.text(ctx, c, struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{"error", message})
	_ = c.Close(websocket.StatusPolicyViolation, message)
}
