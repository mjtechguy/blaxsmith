package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// adminService serves the owner/admin operations dashboard. Reads require a
// live owner/admin session; mutations also require CSRF and recheck the
// session and role under lock in the store.
type adminService struct {
	guard      *identity.BrowserGuard
	store      *workflow.Store
	workerPool string
	// ponytail: the app cannot read AX pool replicas; operators may set
	// BLAXSMITH_ADMIN_ATTEMPT_CAPACITY to show in-flight attempts against a max.
	capacity int32
}

func newAdminService(guard *identity.BrowserGuard, store *workflow.Store, workerPool string) *adminService {
	capacity, _ := strconv.ParseInt(os.Getenv("BLAXSMITH_ADMIN_ATTEMPT_CAPACITY"), 10, 32)
	return &adminService{guard: guard, store: store, workerPool: workerPool, capacity: int32(max(capacity, 0))}
}

func adminTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func adminOptionalTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return adminTime(*t)
}

func adminError(err error) error {
	if errors.Is(err, workflow.ErrAdminDenied) || errors.Is(err, workflow.ErrProjectModelAccessDenied) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("organization administration denied"))
	}
	return workflowError(err)
}

func (s *adminService) GetAdminOverview(ctx context.Context, req *connect.Request[api.GetAdminOverviewRequest]) (*connect.Response[api.GetAdminOverviewResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	overview, err := s.store.AdminOverview(ctx, caller)
	if err != nil {
		return nil, adminError(err)
	}
	response := &api.GetAdminOverviewResponse{GeneratedAt: adminTime(time.Now()),
		Capacity: &api.AdminCapacity{InFlight: overview.Capacity.InFlight, Running: overview.Capacity.Running,
			TakenOver: overview.Capacity.TakenOver, ConfiguredMax: s.capacity, WorkerPool: s.workerPool}}
	for _, a := range overview.LiveAttempts {
		response.LiveAttempts = append(response.LiveAttempts, &api.AdminLiveAttempt{AttemptId: a.AttemptID, RunId: a.RunID,
			ProjectId: a.ProjectID, ProjectName: a.ProjectName, RunLaunchKey: a.LaunchKey, Stage: a.Stage, Kind: a.Kind,
			Harness: a.Harness, Model: a.Model, State: a.State, ControllerPrincipalId: a.ControllerID,
			ControllerUsername: a.ControllerUsername, StartedAt: adminTime(a.StartedAt), LastActivityAt: adminOptionalTime(a.LastActivity)})
	}
	for _, i := range overview.OpenInteractions {
		response.OpenInteractions = append(response.OpenInteractions, &api.AdminOpenInteraction{Id: i.ID, RunId: i.RunID,
			ProjectId: i.ProjectID, ProjectName: i.ProjectName, RunLaunchKey: i.LaunchKey, Stage: i.Stage, Kind: i.Kind,
			Title: i.Title, Blocking: i.Blocking, CreatedAt: adminTime(i.CreatedAt)})
	}
	for _, state := range []string{"running", "waiting_on_human", "escalated", "failed", "succeeded", "halted"} {
		response.RunStates = append(response.RunStates, &api.AdminRunStateCount{State: state, Count: overview.RunStates[state]})
	}
	for _, c := range overview.Connections {
		response.Connections = append(response.Connections, &api.AdminConnection{Id: c.ID, ProviderKind: c.ProviderKind,
			Host: c.Host, Account: c.Account, OwnerKind: c.OwnerKind, State: c.State, ActiveGrants: c.ActiveGrants,
			ActiveLeases: c.ActiveLeases, ExpiringLeases: c.ExpiringLeases, LastUsedAt: adminOptionalTime(c.LastUsed),
			CreatedAt: adminTime(c.CreatedAt)})
	}
	for _, g := range overview.Grants {
		response.Grants = append(response.Grants, &api.AdminGrant{Id: g.ID, ConnectionId: g.ConnectionID, ProjectId: g.ProjectID,
			ProjectName: g.ProjectName, Capability: g.Capability, Resource: g.Resource,
			ExpiresAt: adminOptionalTime(g.ExpiresAt), CreatedAt: adminTime(g.CreatedAt)})
	}
	return connect.NewResponse(response), nil
}

func (s *adminService) ListAuditEvents(ctx context.Context, req *connect.Request[api.ListAuditEventsRequest]) (*connect.Response[api.ListAuditEventsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.PageSize)
	if err != nil {
		return nil, err
	}
	// The token is the last returned id; filters are resent by the client.
	var before int64
	if req.Msg.PageToken != "" {
		raw, decodeErr := base64.RawURLEncoding.DecodeString(req.Msg.PageToken)
		if before, err = strconv.ParseInt(string(raw), 10, 64); decodeErr != nil || err != nil || before < 1 {
			return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
		}
	}
	events, err := s.store.ListAuditEvents(ctx, caller, workflow.AuditFilter{Action: req.Msg.Action, Actor: req.Msg.Actor,
		ProjectID: req.Msg.ProjectId, BeforeID: before, Limit: size + 1})
	if err != nil {
		return nil, adminError(err)
	}
	response := &api.ListAuditEventsResponse{}
	if len(events) > size {
		events = events[:size]
		response.NextPageToken = base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(events[size-1].ID, 10)))
	}
	for _, e := range events {
		response.Events = append(response.Events, &api.AdminAuditEvent{Id: e.ID, Action: e.Action, ActorKind: e.ActorKind,
			ActorId: e.ActorID, ActorUsername: e.ActorUsername, SubjectId: e.SubjectID, ProjectId: e.ProjectID,
			ProjectName: e.Name, OccurredAt: adminTime(e.OccurredAt)})
	}
	return connect.NewResponse(response), nil
}

func (s *adminService) ListAuditActions(ctx context.Context, req *connect.Request[api.ListAuditActionsRequest]) (*connect.Response[api.ListAuditActionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	actions, err := s.store.ListAuditActions(ctx, caller)
	if err != nil {
		return nil, adminError(err)
	}
	return connect.NewResponse(&api.ListAuditActionsResponse{Actions: actions}), nil
}

func (s *adminService) HaltRun(ctx context.Context, req *connect.Request[api.HaltRunRequest]) (*connect.Response[api.HaltRunResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	state, err := s.store.HaltRunAs(ctx, caller, req.Msg.RunId)
	if err != nil {
		return nil, adminError(err)
	}
	return connect.NewResponse(&api.HaltRunResponse{RunId: req.Msg.RunId, State: state}), nil
}

func (s *adminService) RevokeGrant(ctx context.Context, req *connect.Request[api.RevokeGrantRequest]) (*connect.Response[api.RevokeGrantResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.RevokeGrantAs(ctx, caller, req.Msg.GrantId); err != nil {
		return nil, adminError(err)
	}
	return connect.NewResponse(&api.RevokeGrantResponse{GrantId: req.Msg.GrantId}), nil
}
