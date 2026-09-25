package main

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"time"

	"connectrpc.com/connect"
	"github.com/coder/websocket"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const terminalSocketLimit = 64

// appGuestRouter reads the in-cluster atenet-router HTTPS host:port, its CA
// and the connector token file. Unset disables terminals and takeover; set
// without the CA or token fails startup. There is no kubeconfig,
// port-forward, or plaintext fallback.
func appGuestRouter() (*terminal.Router, error) {
	address := os.Getenv("BLAXSMITH_GUEST_ROUTER")
	if address == "" {
		return nil, nil
	}
	return terminal.NewRouter(address, os.Getenv("BLAXSMITH_GUEST_ROUTER_CA_FILE"),
		os.Getenv("BLAXSMITH_GUEST_ROUTER_TOKEN_FILE"))
}

// terminalHandler serves GET /api/terminal/attempts/{attemptID}.
// coder/websocket: maintained, context-native, stdlib net/http, no deps.
type terminalHandler struct {
	guard   *identity.BrowserGuard
	origin  string
	store   *workflow.Store
	router  *terminal.Router
	hub     *terminal.Hub
	slots   chan struct{}
	recheck time.Duration
}

func newTerminalHandler(guard *identity.BrowserGuard, origin string, store *workflow.Store, router *terminal.Router, hub *terminal.Hub) *terminalHandler {
	return &terminalHandler{guard: guard, origin: origin, store: store, router: router, hub: hub,
		slots: make(chan struct{}, terminalSocketLimit)}
}

func (h *terminalHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// Strict: browsers always send Origin on WebSocket upgrades, so a missing
	// or different one is cross-site (CSWSH) or not a browser.
	if origins := r.Header.Values("Origin"); len(origins) != 1 || origins[0] != h.origin {
		http.Error(w, "request origin denied", http.StatusForbidden)
		return
	}
	if h.router == nil {
		http.Error(w, "terminals unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "too many terminals", http.StatusServiceUnavailable)
		return
	}
	caller, err := h.guard.StreamCaller(r.Context(), r.Header)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	attemptID := r.PathValue("attemptID")
	target, err := h.store.GetAttemptTerminal(r.Context(), caller.OrganizationID, attemptID)
	if errors.Is(err, workflow.ErrNotFound) || errors.Is(err, workflow.ErrInvalid) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "terminal unavailable", http.StatusServiceUnavailable)
		return
	}
	guest, err := h.router.Guest(target.Atespace, target.Actor)
	if err != nil {
		http.Error(w, "attempt has no running terminal", http.StatusConflict)
		return
	}
	// http.Server Read/WriteTimeout deadlines survive the hijack; clear them.
	rc := http.NewResponseController(w)
	if rc.SetReadDeadline(time.Time{}) != nil || rc.SetWriteDeadline(time.Time{}) != nil {
		http.Error(w, "terminal unsupported", http.StatusInternalServerError)
		return
	}
	u, _ := url.Parse(h.origin)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{u.Host}})
	if err != nil {
		return
	}
	member := h.hub.Join(attemptID, caller.SessionID)
	defer member.Leave()
	header := r.Header.Clone()
	session := &terminal.Session{Guest: guest, Wake: member.Wake(), Recheck: h.recheck,
		Check: func(ctx context.Context) (terminal.Access, error) {
			current, err := h.guard.StreamCaller(ctx, header)
			if err != nil || current.OrganizationID != caller.OrganizationID || current.PrincipalID != caller.PrincipalID ||
				current.SessionID != caller.SessionID || current.Role != caller.Role {
				return terminal.Access{}, errors.New("session changed")
			}
			now, err := h.store.GetAttemptTerminal(ctx, caller.OrganizationID, attemptID)
			if err != nil || now.Atespace != target.Atespace || now.Actor != target.Actor {
				return terminal.Access{}, errors.New("attempt unavailable")
			}
			return terminalAccess(now, current, member), nil
		}}
	session.Serve(r.Context(), conn)
}

type controlMember interface{ Controls(string) bool }

func terminalAccess(t workflow.AttemptTerminal, caller identity.Caller, member controlMember) terminal.Access {
	state := terminal.State{Type: "state", Control: "agent", Stage: t.Stage, AttemptStatus: t.State}
	if t.Human() {
		holder := t.HolderPrincipalID
		state.Control, state.Holder = "human", &holder
	}
	control := t.Human() && canControlRun(caller.Role) && t.HolderPrincipalID == caller.PrincipalID &&
		member.Controls(t.HolderSessionID)
	// During a human takeover only the controlling socket sees the terminal:
	// the native TUI is unredacted and holds the leased credential.
	return terminal.Access{Control: control, Hidden: t.Human() && !control, State: state}
}

func canControlRun(role string) bool { return role == "owner" || role == "admin" || role == "member" }

func attemptControlMessage(t workflow.AttemptTerminal) *api.AttemptControl {
	message := &api.AttemptControl{AttemptId: t.AttemptID, Control: "agent", Generation: t.ControlGeneration,
		Stage: t.Stage, AttemptStatus: t.State}
	if t.Human() {
		message.Control, message.HolderPrincipalId = "human", t.HolderPrincipalID
	}
	return message
}

func (s *workflowService) GetAttemptControl(ctx context.Context, req *connect.Request[api.GetAttemptControlRequest]) (*connect.Response[api.GetAttemptControlResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	t, err := s.store.GetAttemptTerminal(ctx, caller.OrganizationID, req.Msg.AttemptId)
	if err != nil {
		return nil, workflowError(err)
	}
	canTakeOver, err := s.store.CanTakeOverAttempt(ctx, caller, t.AttemptID)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetAttemptControlResponse{Control: attemptControlMessage(t),
		CanTakeOver: canTakeOver && s.guests != nil}), nil
}

// guestOpTimeout bounds takeover/handback guest work; it outlives the RPC's
// own cancellation so a disconnect cannot stop a takeover halfway.
const guestOpTimeout = 45 * time.Second

func (s *workflowService) TakeOverAttempt(ctx context.Context, req *connect.Request[api.TakeOverAttemptRequest]) (*connect.Response[api.TakeOverAttemptResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if s.guests == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("interactive terminals are not configured"))
	}
	if !canControlRun(caller.Role) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("attempt control denied"))
	}
	t, changed, err := s.store.TakeOverAttempt(ctx, caller, req.Msg.AttemptId)
	if err != nil {
		return nil, workflowError(err)
	}
	if changed {
		s.notifyTerminals(t.AttemptID)
		guest, err := s.guests.Guest(t.Atespace, t.Actor)
		opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guestOpTimeout)
		defer cancel()
		if err == nil {
			err = guest.TakeOver(opCtx)
		}
		if err != nil {
			// The agent was not (or not fully) replaced; give control back.
			_, _ = s.store.ReleaseAttemptControl(opCtx, caller, t.AttemptID, t.ControlGeneration)
			s.notifyTerminals(t.AttemptID)
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("guest takeover failed"))
		}
	}
	return connect.NewResponse(&api.TakeOverAttemptResponse{Control: attemptControlMessage(t)}), nil
}

func (s *workflowService) HandBackAttempt(ctx context.Context, req *connect.Request[api.HandBackAttemptRequest]) (*connect.Response[api.HandBackAttemptResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if s.guests == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("interactive terminals are not configured"))
	}
	t, err := s.store.GetAttemptTerminal(ctx, caller.OrganizationID, req.Msg.AttemptId)
	if err != nil {
		return nil, workflowError(err)
	}
	if t.HolderPrincipalID == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("attempt is not under human control"))
	}
	if t.HolderPrincipalID != caller.PrincipalID {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only the control holder can hand back"))
	}
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), guestOpTimeout)
	defer cancel()
	// Guest first, DB second: bystanders stay hidden until the release commits,
	// so they never see the unredacted human TUI if the guest step fails.
	if t.State == "running" {
		guest, err := s.guests.Guest(t.Atespace, t.Actor)
		if err == nil {
			err = guest.HandBack(opCtx)
		}
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("guest handback failed"))
		}
	}
	t, err = s.store.ReleaseAttemptControl(opCtx, caller, t.AttemptID, t.ControlGeneration)
	if err != nil {
		return nil, workflowError(err)
	}
	s.notifyTerminals(t.AttemptID)
	return connect.NewResponse(&api.HandBackAttemptResponse{Control: attemptControlMessage(t)}), nil
}

func (s *workflowService) notifyTerminals(attemptID string) {
	if s.terminals != nil {
		s.terminals.Notify(attemptID)
	}
}

// Guest result file read by completion before the actor stops. ParseAttemptResult
// rejects anything larger.
// ponytail: an oversized result.json fails the read and the stop retries each
// sweep; bound it guest-side (the pane writes it) rather than here.
const (
	attemptResultPath = "/tmp/blaxsmith/result.json"
	attemptResultMax  = 128 << 10
)

// readAttemptGuestFile reads a bounded file from the attempt's pinned actor
// (Lane A completion reads /tmp/blaxsmith/result.json through this).
func readAttemptGuestFile(ctx context.Context, store *workflow.Store, router *terminal.Router, a workflow.Attempt, path string, max int) ([]byte, error) {
	if router == nil {
		return nil, errors.New("guest router not configured")
	}
	binding, err := store.GetRuntimeBinding(ctx, a)
	if err != nil {
		return nil, err
	}
	guest, err := router.Guest(binding.AXAtespace, binding.AXTask)
	if err != nil {
		return nil, err
	}
	return guest.ReadFile(ctx, path, max)
}
