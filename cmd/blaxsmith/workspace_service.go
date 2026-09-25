package main

import (
	"context"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// workspaceService serves the cross-project shell views. Every read requires a
// live session and origin; the store scopes each to the session organization.
// The member directory is owner/admin only.
type workspaceService struct {
	guard *identity.BrowserGuard
	store *workflow.Store
	users *identity.UserAdmin
}

func workspaceRun(r workflow.WorkspaceRun) *api.WorkspaceRun {
	return &api.WorkspaceRun{Id: r.ID, ProjectId: r.ProjectID, ProjectName: r.ProjectName, LaunchKey: r.LaunchKey,
		SourceCommit: r.SourceCommit, State: r.State, CreatedAt: adminTime(r.CreatedAt), Status: r.Status,
		OpenInteractions: r.OpenInteractions, ReviewWaiting: r.ReviewWaiting, StageCount: r.StageCount,
		StagesSucceeded: r.StagesSucceeded}
}

func inboxItem(i workflow.InboxItem) *api.InboxItem {
	return &api.InboxItem{Id: i.ID, Kind: i.Kind, RunId: i.RunID, ProjectId: i.ProjectID, ProjectName: i.ProjectName,
		RunLaunchKey: i.LaunchKey, Stage: i.Stage, Title: i.Title, Blocking: i.Blocking, CreatedAt: adminTime(i.CreatedAt),
		CanAct: i.CanAct}
}

func (s *workspaceService) GetWorkspaceHome(ctx context.Context, req *connect.Request[api.GetWorkspaceHomeRequest]) (*connect.Response[api.GetWorkspaceHomeResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	home, err := s.store.WorkspaceHome(ctx, caller)
	if err != nil {
		return nil, adminError(err)
	}
	response := &api.GetWorkspaceHomeResponse{WaitingOnYou: home.WaitingOnYou, OpenItems: home.OpenItems,
		RunningAgents: home.RunningAgents, ActiveRuns: home.ActiveRuns, RunsLast_24H: home.RunsLast24h,
		FailedLast_24H: home.FailedLast24h, GeneratedAt: adminTime(time.Now()), OrganizationName: home.OrganizationName,
		OrganizationSlug: home.OrganizationSlug, Username: home.Username, DisplayName: home.DisplayName, Email: home.Email}
	for _, i := range home.Waiting {
		response.Waiting = append(response.Waiting, inboxItem(i))
	}
	for _, a := range home.Agents {
		response.Agents = append(response.Agents, &api.WorkspaceAgent{AttemptId: a.AttemptID, RunId: a.RunID,
			ProjectId: a.ProjectID, ProjectName: a.ProjectName, RunLaunchKey: a.LaunchKey, Stage: a.Stage, Kind: a.Kind,
			Harness: a.Harness, Model: a.Model, State: a.State, StartedAt: adminTime(a.StartedAt),
			LastActivityAt: adminOptionalTime(a.LastActivity), TakenOver: a.ControllerID != ""})
	}
	for _, r := range home.RecentRuns {
		response.RecentRuns = append(response.RecentRuns, workspaceRun(r))
	}
	return connect.NewResponse(response), nil
}

func (s *workspaceService) ListInbox(ctx context.Context, req *connect.Request[api.ListInboxRequest]) (*connect.Response[api.ListInboxResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	items, total, err := s.store.ListInbox(ctx, caller, workflow.InboxFilter{Page: int(m.Page), PageSize: int(m.PageSize),
		Kinds: m.Kinds, ProjectID: m.ProjectId, Search: m.Search, ActionableOnly: m.ActionableOnly})
	if err != nil {
		return nil, adminError(err)
	}
	response := &api.ListInboxResponse{TotalCount: total}
	for _, i := range items {
		response.Items = append(response.Items, inboxItem(i))
	}
	return connect.NewResponse(response), nil
}

func (s *workspaceService) ListWorkspaceRuns(ctx context.Context, req *connect.Request[api.ListWorkspaceRunsRequest]) (*connect.Response[api.ListWorkspaceRunsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	runs, total, err := s.store.ListWorkspaceRuns(ctx, caller, workflow.RunFilter{Page: int(m.Page), PageSize: int(m.PageSize),
		Search: m.Search, States: m.States, ProjectID: m.ProjectId, SortBy: m.SortBy, SortDirection: m.SortDirection})
	if err != nil {
		return nil, adminError(err)
	}
	response := &api.ListWorkspaceRunsResponse{TotalCount: total}
	for _, r := range runs {
		response.Runs = append(response.Runs, workspaceRun(r))
	}
	return connect.NewResponse(response), nil
}

func (s *workspaceService) ListMembersPage(ctx context.Context, req *connect.Request[api.ListMembersPageRequest]) (*connect.Response[api.ListMembersPageResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	m := req.Msg
	members, total, err := s.users.ListMembersPage(ctx, caller, identity.MemberFilter{Page: int(m.Page),
		PageSize: int(m.PageSize), Search: m.Search, Roles: m.Roles, Statuses: m.Statuses, SortBy: m.SortBy,
		SortDirection: m.SortDirection})
	if err != nil {
		return nil, userAdminError(err)
	}
	response := &api.ListMembersPageResponse{TotalCount: total}
	for _, m := range members {
		response.Members = append(response.Members, orgMember(m))
	}
	return connect.NewResponse(response), nil
}
