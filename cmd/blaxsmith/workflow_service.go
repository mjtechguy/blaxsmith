package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	Key       string    `json:"k,omitempty"`
	Scope     string    `json:"s,omitempty"`
}

func (s *workflowService) CreateProject(ctx context.Context, req *connect.Request[api.CreateProjectRequest]) (*connect.Response[api.CreateProjectResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if caller.Role != "owner" && caller.Role != "admin" && caller.Role != "member" {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("project creation denied"))
	}
	id, err := s.store.CreateProjectAs(ctx, caller, req.Msg.Slug, strings.TrimSpace(req.Msg.Name))
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

func (s *workflowService) GetProjectSource(ctx context.Context, req *connect.Request[api.GetProjectSourceRequest]) (*connect.Response[api.GetProjectSourceResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	source, err := s.store.GetProjectSource(ctx, caller.OrganizationID, req.Msg.ProjectId)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetProjectSourceResponse{Source: projectSourceMessage(source)}), nil
}

func (s *workflowService) SetProjectSource(ctx context.Context, req *connect.Request[api.SetProjectSourceRequest]) (*connect.Response[api.SetProjectSourceResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	source, err := s.store.SetProjectSourceAs(ctx, caller, req.Msg.ProjectId, req.Msg.RepositoryUrl, req.Msg.Ref)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.SetProjectSourceResponse{Source: projectSourceMessage(source)}), nil
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
	page, scope, err := listPage(req.Msg.PageToken, caller.OrganizationID, "", req.Msg.Search, req.Msg.SortBy, req.Msg.SortDirection, size, "name")
	if err != nil {
		return nil, err
	}
	projects, err := s.store.ListProjects(ctx, caller.OrganizationID, page)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListProjectsResponse{}
	if len(projects) > size {
		projects = projects[:size]
		last := projects[len(projects)-1]
		response.NextPageToken = encodeCursor(last.CreatedAt, last.Name, last.ID, scope)
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

func (s *workflowService) ListRunTasks(ctx context.Context, req *connect.Request[api.ListRunTasksRequest]) (*connect.Response[api.ListRunTasksResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetRun(ctx, caller.OrganizationID, req.Msg.RunId); err != nil {
		return nil, workflowError(err)
	}
	tasks, err := s.store.ListRunTasks(ctx, caller.OrganizationID, req.Msg.RunId)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListRunTasksResponse{}
	for _, task := range tasks {
		item := &api.RunTask{Id: task.ID, Key: task.Key, State: task.State, Generation: task.Generation,
			MaxAttempts: task.MaxAttempts, DependsOn: task.DependsOn}
		if task.ActiveAttemptID != nil {
			item.ActiveAttemptId = *task.ActiveAttemptID
		}
		response.Tasks = append(response.Tasks, item)
	}
	return connect.NewResponse(response), nil
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
	page, scope, err := listPage(req.Msg.PageToken, caller.OrganizationID, req.Msg.ProjectId, req.Msg.Search, req.Msg.SortBy, req.Msg.SortDirection, size, "launch_key", "state")
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetProject(ctx, caller.OrganizationID, req.Msg.ProjectId); err != nil {
		return nil, workflowError(err)
	}
	runs, err := s.store.ListRuns(ctx, caller.OrganizationID, req.Msg.ProjectId, page)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListRunsResponse{}
	if len(runs) > size {
		runs = runs[:size]
		last := runs[len(runs)-1]
		key := last.LaunchKey
		if page.Sort == "state" {
			key = last.State
		}
		response.NextPageToken = encodeCursor(last.CreatedAt, key, last.ID, scope)
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

func (s *workflowService) ListCommandExits(ctx context.Context, req *connect.Request[api.ListCommandExitsRequest]) (*connect.Response[api.ListCommandExitsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetRun(ctx, caller.OrganizationID, req.Msg.RunId); err != nil {
		return nil, workflowError(err)
	}
	limit := req.Msg.Limit
	if limit == 0 {
		limit = 50
	}
	if req.Msg.AfterEventId < 0 || limit < 1 || limit > 100 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid command exit cursor or limit"))
	}
	observations, err := s.store.ListCommandExits(ctx, caller.OrganizationID, req.Msg.RunId, req.Msg.AfterEventId, int(limit))
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListCommandExitsResponse{NextAfterEventId: req.Msg.AfterEventId}
	for _, observation := range observations {
		response.Observations = append(response.Observations, &api.CommandExitObservation{
			EventId: observation.EventID, TaskId: observation.TaskID, AttemptId: observation.AttemptID,
			ActorUid: observation.ActorUID, SignerId: observation.SignerID, ReceiptSha256: observation.ReceiptSHA256,
			ExitCode: observation.ExitCode, Signal: observation.Signal,
			Interrupted: observation.Interrupted, ObservedAtUnixNanos: observation.ObservedAt,
			ReceivedAt: observation.ReceivedAt.UTC().Format(time.RFC3339Nano),
		})
		response.NextAfterEventId = observation.EventID
	}
	return connect.NewResponse(response), nil
}

func (s *workflowService) GetCurrentReview(ctx context.Context, req *connect.Request[api.GetCurrentReviewRequest]) (*connect.Response[api.GetCurrentReviewResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	current, err := s.store.GetCurrentReview(ctx, caller.OrganizationID, req.Msg.RunId)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetCurrentReviewResponse{Package: reviewPackageMessage(current)}), nil
}

func (s *workflowService) DecideReview(ctx context.Context, req *connect.Request[api.DecideReviewRequest]) (*connect.Response[api.DecideReviewResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	decision, err := s.store.DecideReview(ctx, caller, req.Msg.RunId, req.Msg.PackageId, req.Msg.IdempotencyKey, req.Msg.Action, req.Msg.Feedback)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.DecideReviewResponse{Decision: reviewDecisionMessage(decision)}), nil
}

func reviewPackageMessage(current workflow.ReviewPackage) *api.ReviewPackage {
	message := &api.ReviewPackage{Id: current.ID, RunId: current.RunID, Revision: current.Revision,
		SourceCommit: current.SourceCommit, BundleSha256: current.BundleSHA256,
		VerificationSha256: current.VerificationSHA256, IntegratedCommit: current.IntegratedCommit,
		EvidenceSha256: current.EvidenceSHA256, PresentedAt: current.PresentedAt.UTC().Format(time.RFC3339Nano)}
	if current.Decision != nil {
		message.Decision = reviewDecisionMessage(*current.Decision)
	}
	return message
}

func reviewDecisionMessage(decision workflow.ReviewDecision) *api.ReviewDecision {
	return &api.ReviewDecision{Id: decision.ID, PackageId: decision.PackageID,
		PrincipalId: decision.PrincipalID, Action: decision.Action, Feedback: decision.Feedback,
		DecidedAt: decision.DecidedAt.UTC().Format(time.RFC3339Nano)}
}

func projectMessage(project workflow.Project) *api.Project {
	return &api.Project{Id: project.ID, Slug: project.Slug, Name: project.Name,
		CreatedAt: project.CreatedAt.UTC().Format(time.RFC3339Nano)}
}

func projectSourceMessage(source workflow.ProjectSource) *api.ProjectSource {
	return &api.ProjectSource{ProjectId: source.ProjectID, RepositoryUrl: source.RepositoryURL,
		Ref: source.Ref, UpdatedAt: source.UpdatedAt.UTC().Format(time.RFC3339Nano)}
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

func listPage(raw, orgID, projectID, search, sortBy, direction string, size int, extraSorts ...string) (workflow.ListPage, string, error) {
	search = strings.TrimSpace(search)
	if len(search) > 120 {
		return workflow.ListPage{}, "", connect.NewError(connect.CodeInvalidArgument, errors.New("search is too long"))
	}
	if sortBy == "" {
		sortBy = "created_at"
	}
	if direction == "" {
		direction = "desc"
	}
	validSort := sortBy == "created_at"
	for _, allowed := range extraSorts {
		validSort = validSort || sortBy == allowed
	}
	if !validSort || (direction != "asc" && direction != "desc") {
		return workflow.ListPage{}, "", connect.NewError(connect.CodeInvalidArgument, errors.New("invalid list sorting"))
	}
	hash := sha256.Sum256([]byte(orgID + "\x00" + projectID + "\x00" + search + "\x00" + sortBy + "\x00" + direction))
	scope := hex.EncodeToString(hash[:])
	cursor, err := decodeCursor(raw, scope, sortBy)
	if err != nil {
		return workflow.ListPage{}, "", err
	}
	return workflow.ListPage{Search: search, Sort: sortBy, Direction: direction, AfterKey: cursor.Key,
		AfterID: cursor.ID, AfterTime: cursor.CreatedAt, Limit: size + 1}, scope, nil
}

func decodeCursor(raw, scope, sortBy string) (pageCursor, error) {
	if raw == "" {
		return pageCursor{}, nil
	}
	if len(raw) > 2048 {
		return pageCursor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return pageCursor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
	}
	var cursor pageCursor
	if json.Unmarshal(data, &cursor) != nil || cursor.Scope != scope || len(cursor.ID) != 36 || len(cursor.Key) > 640 ||
		(sortBy == "created_at" && cursor.CreatedAt.IsZero()) || (sortBy != "created_at" && cursor.Key == "") {
		return pageCursor{}, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid page token"))
	}
	return cursor, nil
}

func encodeCursor(at time.Time, key, id, scope string) string {
	data, _ := json.Marshal(pageCursor{CreatedAt: at.UTC(), Key: key, ID: id, Scope: scope})
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
	case errors.Is(err, workflow.ErrProjectDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("project creation denied"))
	case errors.Is(err, workflow.ErrProjectSourceDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("project source change denied"))
	case errors.Is(err, workflow.ErrSourceRoute):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("repository host has no verified public route"))
	case errors.Is(err, workflow.ErrReviewDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("human review denied"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("workflow unavailable"))
	}
}
