package main

import (
	"context"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// setupService serves read-only setup helpers; every call needs a live session.
type setupService struct {
	guard    *identity.BrowserGuard
	store    *workflow.Store
	workflow *workflowService // Project source fetch for repository inspection.
}

func (s *setupService) ExplainAccess(ctx context.Context, req *connect.Request[api.ExplainAccessRequest]) (*connect.Response[api.ExplainAccessResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	e, err := s.store.ExplainAccessAs(ctx, caller, req.Msg.ProjectId, req.Msg.ResourceKind, req.Msg.ResourceId, req.Msg.PrincipalId)
	if err != nil {
		return nil, connectionError(err)
	}
	out := &api.ExplainAccessResponse{Usable: e.Usable, Reason: e.Reason, GrantId: e.GrantID, PrincipalLabel: e.PrincipalLabel}
	for _, step := range e.Steps {
		out.Steps = append(out.Steps, &api.AccessStep{Kind: step.Kind, Label: step.Label, Id: step.ID})
	}
	return connect.NewResponse(out), nil
}
