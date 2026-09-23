package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type workflowService struct {
	guard *identity.BrowserGuard
	store *workflow.Store
}

type pageCursor struct {
	CreatedAt time.Time `json:"t"`
	ID        string    `json:"id"`
}

var firstPage = pageCursor{CreatedAt: time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), ID: "ffffffff-ffff-ffff-ffff-ffffffffffff"}

func (s *workflowService) CreateProject(ctx context.Context, req *connect.Request[api.CreateProjectRequest]) (*connect.Response[api.CreateProjectResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if caller.Role != "owner" && caller.Role != "admin" && caller.Role != "member" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("project creation denied"))
	}
	id, err := s.store.CreateProject(ctx, caller.OrganizationID, req.Msg.Slug, strings.TrimSpace(req.Msg.Name))
	if err != nil {
		var dbErr *pgconn.PgError
		if errors.As(err, &dbErr) && dbErr.Code == "23505" {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("project slug already exists"))
		}
		return nil, workflowError(err)
	}
	project, err := s.store.GetProject(ctx, caller.OrganizationID, id)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.CreateProjectResponse{Project: projectMessage(project)}), nil
}

func (s *workflowService) GetProject(ctx context.Context, req *connect.Request[api.GetProjectRequest]) (*connect.Response[api.GetProjectResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	project, err := s.store.GetProject(ctx, caller.OrganizationID, req.Msg.ProjectId)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetProjectResponse{Project: projectMessage(project)}), nil
}

func (s *workflowService) ListProjects(ctx context.Context, req *connect.Request[api.ListProjectsRequest]) (*connect.Response[api.ListProjectsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.PageSize)
	if err != nil {
		return nil, err
	}
	cursor, err := decodeCursor(req.Msg.PageToken)
	if err != nil {
		return nil, err
	}
	projects, err := s.store.ListProjects(ctx, caller.OrganizationID, cursor.CreatedAt, cursor.ID, size+1)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListProjectsResponse{}
	if len(projects) > size {
		projects = projects[:size]
		response.NextPageToken = encodeCursor(projects[len(projects)-1].CreatedAt, projects[len(projects)-1].ID)
	}
	for _, project := range projects {
		response.Projects = append(response.Projects, projectMessage(project))
	}
	return connect.NewResponse(response), nil
}

func (s *workflowService) GetRun(ctx context.Context, req *connect.Request[api.GetRunRequest]) (*connect.Response[api.GetRunResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	run, err := s.store.GetRun(ctx, caller.OrganizationID, req.Msg.RunId)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetRunResponse{Run: runMessage(run)}), nil
}

func (s *workflowService) ListRuns(ctx context.Context, req *connect.Request[api.ListRunsRequest]) (*connect.Response[api.ListRunsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.PageSize)
	if err != nil {
		return nil, err
	}
	cursor, err := decodeCursor(req.Msg.PageToken)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, caller.OrganizationID, req.Msg.ProjectId); err != nil {
		return nil, workflowError(err)
	}
	runs, err := s.store.ListRuns(ctx, caller.OrganizationID, req.Msg.ProjectId, cursor.CreatedAt, cursor.ID, size+1)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListRunsResponse{}
	if len(runs) > size {
		runs = runs[:size]
		response.NextPageToken = encodeCursor(runs[len(runs)-1].CreatedAt, runs[len(runs)-1].ID)
	}
	for _, run := range runs {
		response.Runs = append(response.Runs, runMessage(run))
	}
	return connect.NewResponse(response), nil
}

func (s *workflowService) EventsAfter(ctx context.Context, req *connect.Request[api.EventsAfterRequest]) (*connect.Response[api.EventsAfterResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetRun(ctx, caller.OrganizationID, req.Msg.RunId); err != nil {
		return nil, workflowError(err)
	}
	limit := req.Msg.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 || req.Msg.AfterId < 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid event cursor or limit"))
	}
	events, err := s.store.EventsAfter(ctx, caller.OrganizationID, req.Msg.RunId, req.Msg.AfterId, int(limit))
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.EventsAfterResponse{NextAfterId: req.Msg.AfterId}
	for _, event := range events {
		message := &api.WorkflowEvent{Id: event.ID, RunId: event.RunID, Kind: event.Kind,
			OccurredAt: event.OccurredAt.UTC().Format(time.RFC3339Nano)}
		if event.TaskID != nil {
			message.TaskId = *event.TaskID
		}
		if event.AttemptID != nil {
			message.AttemptId = *event.AttemptID
		}
		response.Events = append(response.Events, message)
		response.NextAfterId = event.ID
	}
	return connect.NewResponse(response), nil
}

func projectMessage(project workflow.Project) *api.Project {
	return &api.Project{Id: project.ID, Slug: project.Slug, Name: project.Name,
		CreatedAt: project.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func runMessage(run workflow.Run) *api.Run {
	return &api.Run{Id: run.ID, ProjectId: run.ProjectID, LaunchKey: run.LaunchKey,
		SourceCommit: run.SourceCommit, BundleSha256: run.BundleSHA256,
		VerificationSha256: run.VerificationSHA256, State: run.State,
		CreatedAt: run.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func pageSize(value int32) (int, error) {
	if value == 0 {
		return 50, nil
	}
	if value < 1 || value > 100 {
		return 0, connect.NewError(connect.CodeInvalidArgument, errors.New("page size must be 1-100"))
	}
	return int(value), nil
}

func decodeCursor(raw string) (pageCursor, error) {
	if raw == "" {
		return firstPage, nil
	}
	if len(raw) > 256 {
		return pageCursor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return pageCursor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
	}
	var cursor pageCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.CreatedAt.IsZero() || len(cursor.ID) != 36 {
		return pageCursor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
	}
	return cursor, nil
}

func encodeCursor(at time.Time, id string) string {
	data, _ := json.Marshal(pageCursor{CreatedAt: at.UTC(), ID: id})
	return base64.RawURLEncoding.EncodeToString(data)
}

func workflowError(err error) error {
	switch {
	case errors.Is(err, workflow.ErrInvalid):
		return connect.NewError(connect.CodeInvalidArgument, errors.New("invalid workflow input"))
	case errors.Is(err, workflow.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("workflow resource not found"))
	case errors.Is(err, workflow.ErrConflict):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("workflow state conflict"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("workflow unavailable"))
	}
}
