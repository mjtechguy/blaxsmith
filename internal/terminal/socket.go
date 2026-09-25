package terminal

import (
	"context"
	"encoding/json"
	"errors"
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
	// Hidden withholds terminal bytes: while a human controls the session the
	// native TUI runs unredacted with the leased credential in its environment,
	// so only the controlling socket may see it. Viewers get state frames only
	// until control returns to the agent (whose rendered output is redacted).
	Hidden bool
	State  State
}

// Session serves one browser socket. Check must revalidate the session
// cookie, tenant/attempt authorization, and holder on every call; an error
// closes the socket with a safe message.
type Session struct {
	Guest   *Guest
	Check   func(context.Context) (Access, error)
	Wake    <-chan struct{}
	Recheck time.Duration // default 10s
	Window  Window        // zero fields take the defaults below
}

// Window is the acknowledgement-based output window: at most Chunks binary
// frames or Bytes bytes are unacknowledged on the wire, and at most Buffer
// bytes wait behind them before the viewer is dropped with an error frame.
type Window struct{ Chunks, Bytes, Buffer int }

const (
	maxClientFrame = 64 << 10
	writeTimeout   = 10 * time.Second
	windowChunks   = 8
	windowBytes    = 64 << 10
	windowBuffer   = 1 << 20
	// ErrSlowViewer is the error frame message when a viewer falls behind.
	ErrSlowViewer = "terminal output overflowed; the viewer fell behind"
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

	out := newOutbox(s.Window)
	go func() { // the only writer of binary frames; it waits on the window
		for {
			data, err := out.next(ctx)
			if err != nil {
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, writeTimeout)
			err = c.Write(wctx, websocket.MessageBinary, data)
			wcancel()
			if err != nil {
				cancel()
				return
			}
		}
	}()

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
				Bytes      int `json:"bytes"`
			}
			if json.Unmarshal(data, &frame) == nil && frame.Type == "ack" {
				out.ack(frame.Bytes)
				continue
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
		if access.Hidden {
			if !s.waitVisible(ctx, c, &access, ticker.C) {
				return
			}
			continue
		}
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
				if !out.push(data) {
					done <- attachResult{nil, errOverflow}
					return
				}
			}
		}()
		reattach, closeMsg := s.pump(ctx, c, att, out, &access, input, resize, done, ticker.C)
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
func (s *Session) pump(ctx context.Context, c *websocket.Conn, att *Attachment, out *outbox, access *Access,
	input <-chan []byte, resize <-chan [2]int, done <-chan attachResult, tick <-chan time.Time) (bool, *string) {
	for {
		recheck := false
		select {
		case <-ctx.Done():
			return false, nil
		case r := <-done:
			if errors.Is(r.err, errOverflow) {
				s.fail(ctx, c, ErrSlowViewer)
				return false, nil
			}
			if r.exit != nil {
				// Output queued before the exit reaches the viewer first.
				dctx, dcancel := context.WithTimeout(ctx, writeTimeout)
				out.drain(dctx)
				dcancel()
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
		changed := stateChanged(access.State, next.State)
		mode := next.Control != access.Control || next.Hidden != access.Hidden
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

func stateChanged(a, b State) bool {
	return a.Control != b.Control || a.AttemptStatus != b.AttemptStatus || a.Stage != b.Stage ||
		(a.Holder == nil) != (b.Holder == nil) || (a.Holder != nil && b.Holder != nil && *a.Holder != *b.Holder)
}

// waitVisible holds a hidden viewer without any attachment, relaying state
// changes, until the session is visible again. It reports false when the
// socket should close (context done, access revoked, or a write failed).
// ponytail: other replicas notice a takeover only on their recheck tick; with
// several app replicas, lower Recheck or fan Wake out across replicas.
func (s *Session) waitVisible(ctx context.Context, c *websocket.Conn, access *Access, tick <-chan time.Time) bool {
	for access.Hidden {
		select {
		case <-ctx.Done():
			return false
		case <-s.Wake:
		case <-tick:
		}
		next, err := s.Check(ctx)
		if err != nil {
			s.fail(ctx, c, "terminal access revoked")
			return false
		}
		changed := stateChanged(access.State, next.State)
		*access = next
		if changed && !s.text(ctx, c, next.State) {
			return false
		}
	}
	return true
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

var errOverflow = errors.New("terminal viewer buffer overflow")

// outbox is one socket's output window (docs/interactive-sessions.md,
// "Flow control"). The attach reader pushes guest bytes; the socket writer
// takes them only while the unacknowledged window has room; the client acks
// the bytes it has rendered. Idea from t3code's terminal OutputProtocol (MIT).
type outbox struct {
	mu      sync.Mutex
	limit   Window
	sent    []int // sizes of unacknowledged frames, oldest first
	inWire  int   // bytes in sent
	credit  int   // acknowledged bytes not yet covering a whole frame
	queue   [][]byte
	queued  int
	wake    chan struct{} // writer: queue or window changed
	drained chan struct{} // drain: the queue emptied
}

func newOutbox(w Window) *outbox {
	if w.Chunks <= 0 {
		w.Chunks = windowChunks
	}
	if w.Bytes <= 0 {
		w.Bytes = windowBytes
	}
	if w.Buffer <= 0 {
		w.Buffer = windowBuffer
	}
	return &outbox{limit: w, wake: make(chan struct{}, 1), drained: make(chan struct{}, 1)}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// push queues guest output; false means the viewer is too far behind.
func (o *outbox) push(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	o.mu.Lock()
	for len(data) > 0 { // bounded frames keep the byte window meaningful
		n := min(len(data), max(o.limit.Bytes/4, 1))
		o.queue = append(o.queue, data[:n])
		o.queued += n
		data = data[n:]
	}
	over := o.queued > o.limit.Buffer
	o.mu.Unlock()
	signal(o.wake)
	return !over
}

// ack credits n rendered bytes and retires every frame they fully cover.
// Credit never exceeds what is on the wire, so acks cannot pre-pay output.
func (o *outbox) ack(n int) {
	if n <= 0 {
		return
	}
	o.mu.Lock()
	o.credit = min(o.credit+n, o.inWire)
	for len(o.sent) > 0 && o.sent[0] <= o.credit {
		o.credit -= o.sent[0]
		o.inWire -= o.sent[0]
		o.sent = o.sent[1:]
	}
	o.mu.Unlock()
	signal(o.wake)
}

// next blocks until a frame may be sent and records it as unacknowledged.
func (o *outbox) next(ctx context.Context) ([]byte, error) {
	for {
		o.mu.Lock()
		if len(o.queue) > 0 && len(o.sent) < o.limit.Chunks && o.inWire < o.limit.Bytes {
			data := o.queue[0]
			o.queue[0] = nil
			o.queue = o.queue[1:]
			o.queued -= len(data)
			o.sent = append(o.sent, len(data))
			o.inWire += len(data)
			empty := len(o.queue) == 0
			o.mu.Unlock()
			if empty {
				signal(o.drained)
			}
			return data, nil
		}
		o.mu.Unlock()
		select {
		case <-o.wake:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// drain waits until every queued frame has been handed to the socket.
func (o *outbox) drain(ctx context.Context) {
	for {
		o.mu.Lock()
		empty := len(o.queue) == 0
		o.mu.Unlock()
		if empty {
			return
		}
		select {
		case <-o.drained:
		case <-ctx.Done():
			return
		}
	}
}
