package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/repoinspect"
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

func commandMessages(cs []repoinspect.Command) []*api.RepositoryCommand {
	out := make([]*api.RepositoryCommand, 0, len(cs))
	for _, c := range cs {
		out = append(out, &api.RepositoryCommand{Id: c.ID, Command: c.Command})
	}
	return out
}

func (s *setupService) InspectRepository(ctx context.Context, req *connect.Request[api.InspectRepositoryRequest]) (*connect.Response[api.InspectRepositoryResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, caller.OrganizationID, req.Msg.ProjectId); err != nil {
		return nil, workflowError(err)
	}
	_, _, fetched, err := s.workflow.fetchProjectSource(ctx, caller.OrganizationID, req.Msg.ProjectId)
	if err != nil {
		return nil, err
	}
	defer fetched.Close()
	commit, files, contents, err := recipe.ReadFiles(ctx, fetched.Directory, fetched.Commit, repoinspect.Wanted)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("repository files could not be read"))
	}
	r := repoinspect.Inspect(files, contents)
	return connect.NewResponse(&api.InspectRepositoryResponse{Commit: commit, Source: r.Source, FileError: r.FileError,
		Verification: commandMessages(r.Verification), Setup: commandMessages(r.Setup), Recipe: r.Recipe, Evidence: r.Evidence}), nil
}
