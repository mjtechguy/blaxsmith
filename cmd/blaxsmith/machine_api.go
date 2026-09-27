package main

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type machineCallerKey struct{}
type machineTokenKey struct{}
type machineResourceKey struct{}
type machineGuard struct {
	sessions *identity.SessionManager
	pool     *pgxpool.Pool
}

func (g machineGuard) Caller(ctx context.Context, _ http.Header, _ bool) (identity.Caller, error) {
	caller, ok := ctx.Value(machineCallerKey{}).(identity.Caller)
	if !ok {
		return identity.Caller{}, connect.NewError(connect.CodeUnauthenticated, errors.New("machine authentication required"))
	}
	return caller, nil
}

type machineOperation struct{ scope, field, resource string }

// This is an explicit projection. New browser operations are denied until their
// project ownership and machine authority have been defined here.
var machineOperations = map[string]machineOperation{
	"DescribeMachineAccess":          {"", "", ""},
	"GetPlatformCapabilities":        {"project.read", "", ""},
	"GetProject":                     {"project.read", "project_id", "project"},
	"GetProjectSource":               {"project.read", "project_id", "project"},
	"ListProjectVerificationHistory": {"project.read", "project_id", "project"},
	"GetProjectVerification":         {"project.read", "project_id", "project"},
	"ListGoals":                      {"project.read", "project_id", "project"},
	"GetProjectModelOptions":         {"project.read", "project_id", "project"},
	"GetRunUsage":                    {"project.read", "run_id", "run"},
	"GetGoalUsage":                   {"project.read", "goal_id", "goal"},
	"ListGoalCheckpoints":            {"project.read", "goal_id", "goal"},
	"RecordGoalCheckpoint":           {"goal.write", "goal_id", "goal"},
	"GetGoalControl":                 {"project.read", "goal_id", "goal"},
	"ControlGoal":                    {"run.control", "goal_id", "goal"},
	"GetGoalAllowance":               {"project.read", "goal_id", "goal"},
	"GetGoal":                        {"project.read", "goal_id", "goal"},
	"GetGoalPlans":                   {"project.read", "goal_id", "goal"},
	"CreateGoal":                     {"goal.write", "project_id", "project"},
	"ReplyGoal":                      {"goal.write", "goal_id", "goal"},
	"SaveGoalPlan":                   {"goal.write", "goal_id", "goal"},
	"StartGoalPlanning":              {"run.launch", "goal_id", "goal"},
	"PreviewRun":                     {"run.launch", "project_id", "project"},
	"LaunchRun":                      {"run.launch", "project_id", "project"},
	"GetLaunchAvailability":          {"project.read", "project_id", "project"},
	"ListRuns":                       {"project.read", "project_id", "project"},
	"GetDeliveryReport":              {"project.read", "run_id", "run"},
	"GetRun":                         {"project.read", "run_id", "run"},
	"ListRunTasks":                   {"project.read", "run_id", "run"},
	"EventsAfter":                    {"project.read", "run_id", "run"},
	"ListCommandExits":               {"project.read", "run_id", "run"},
	"ListRunEvidence":                {"project.read", "run_id", "run"},
	"GetEvidenceContent":             {"project.read", "run_id", "run"},
	"GetCurrentReview":               {"project.read", "run_id", "run"},
	"ListInteractions":               {"project.read", "run_id", "run"},
	"GetAttemptControl":              {"project.read", "attempt_id", "attempt"},
	"AnswerInteraction":              {"run.control", "interaction_id", "interaction"},
	"SteerAttempt":                   {"run.control", "attempt_id", "attempt"},
	"DecideReview":                   {"review.decide", "run_id", "run"},
}

func (g machineGuard) interceptor() connect.UnaryInterceptorFunc {
	return func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			denied := func() (connect.AnyResponse, error) {
				return nil, connect.NewError(connect.CodePermissionDenied, errors.New("operation or project is outside the credential scope"))
			}
			h := req.Header()
			auth := h.Values("Authorization")
			if len(auth) != 1 || !strings.HasPrefix(auth[0], "Bearer ") || h.Get("Cookie") != "" || h.Get("Origin") != "" {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("use a Blaxsmith bearer credential without browser cookies"))
			}
			caller, token, err := g.sessions.AuthenticateAPI(ctx, strings.TrimPrefix(auth[0], "Bearer "))
			if err != nil {
				if errors.Is(err, identity.ErrUnauthenticated) {
					return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("machine credential is invalid or expired"))
				}
				return nil, connect.NewError(connect.CodeUnavailable, errors.New("authentication unavailable"))
			}
			if token.Resource != "" && ctx.Value(machineResourceKey{}) != token.Resource {
				return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("credential was issued for another resource"))
			}
			_, method, ok := strings.Cut(req.Spec().Procedure, "/blaxsmith.api.v1.WorkflowService/")
			if !ok {
				return denied()
			}
			operation, ok := machineOperations[method]
			if !ok || operation.scope != "" && !slices.Contains(token.Scopes, operation.scope) {
				return denied()
			}
			if operation.field != "" {
				message, ok := req.Any().(proto.Message)
				if !ok {
					return denied()
				}
				reflect := message.ProtoReflect()
				field := reflect.Descriptor().Fields().ByName(protoreflect.Name(operation.field))
				if field == nil {
					return denied()
				}
				id := reflect.Get(field).String()
				if id == "" {
					return denied()
				}
				project := id
				queries := map[string]string{
					"goal":        `SELECT project_id FROM workflow_goals WHERE organization_id=$1 AND id::text=$2`,
					"run":         `SELECT project_id FROM workflow_runs WHERE organization_id=$1 AND id::text=$2`,
					"attempt":     `SELECT r.project_id FROM workflow_attempts a JOIN workflow_runs r ON r.organization_id=a.organization_id AND r.id=a.run_id WHERE a.organization_id=$1 AND a.id::text=$2`,
					"interaction": `SELECT r.project_id FROM workflow_interactions i JOIN workflow_runs r ON r.organization_id=i.organization_id AND r.id=i.run_id WHERE i.organization_id=$1 AND i.id::text=$2`,
				}
				if operation.resource != "project" {
					query, ok := queries[operation.resource]
					if !ok {
						return denied()
					}
					err = g.pool.QueryRow(tenant.Org(ctx, caller.OrganizationID), query, caller.OrganizationID, id).Scan(&project)
					if errors.Is(err, pgx.ErrNoRows) {
						return denied()
					}
					if err != nil {
						return nil, connect.NewError(connect.CodeUnavailable, errors.New("resource authorization unavailable"))
					}
				}
				if project != token.ProjectID {
					return denied()
				}
				if method == "AnswerInteraction" && !slices.Contains(token.Scopes, "review.decide") {
					var kind string
					if err := g.pool.QueryRow(tenant.Org(ctx, caller.OrganizationID), `SELECT kind FROM workflow_interactions WHERE organization_id=$1 AND id::text=$2`, caller.OrganizationID, id).Scan(&kind); err != nil || kind == "approval" {
						return denied()
					}
				}
			}
			if operation.scope != "" && operation.scope != "project.read" {
				// Attribution records receipt of a delegated mutation request, not
				// a claim that its subsequent domain operation succeeded.
				if _, err := g.pool.Exec(tenant.Org(ctx, caller.OrganizationID), `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,$3,$4)`, caller.OrganizationID, caller.PrincipalID, "identity.api.request."+method, token.ID); err != nil {
					return nil, connect.NewError(connect.CodeUnavailable, errors.New("request attribution unavailable"))
				}
			}
			ctx = context.WithValue(ctx, machineTokenKey{}, token)
			return next(context.WithValue(ctx, machineCallerKey{}, caller), req)
		}
	}
}

func (s *workflowService) DescribeMachineAccess(ctx context.Context, req *connect.Request[api.DescribeMachineAccessRequest]) (*connect.Response[api.DescribeMachineAccessResponse], error) {
	token, ok := ctx.Value(machineTokenKey{}).(identity.APIToken)
	if !ok {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("use the machine API with a scoped credential"))
	}
	return connect.NewResponse(&api.DescribeMachineAccessResponse{Credential: apiTokenMessage(token)}), nil
}

func apiTokenMessage(t identity.APIToken) *api.ApiToken {
	out := &api.ApiToken{PrincipalId: t.PrincipalID, Kind: t.Kind, Resource: t.Resource, Id: t.ID, ProjectId: t.ProjectID, Label: t.Label, Scopes: t.Scopes, ExpiresAt: t.ExpiresAt.Format(time.RFC3339)}
	if t.RevokedAt != nil {
		out.RevokedAt = t.RevokedAt.Format(time.RFC3339)
	}
	return out
}
func (s *workflowService) CreateApiToken(ctx context.Context, req *connect.Request[api.CreateApiTokenRequest]) (*connect.Response[api.CreateApiTokenResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if req.Msg.LifetimeSeconds < 3600 || req.Msg.LifetimeSeconds > 30*24*3600 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("credential lifetime must be 1 hour to 30 days"))
	}
	token, raw, err := s.sessions.IssueAPIToken(ctx, caller, req.Msg.ProjectId, req.Msg.Label, req.Msg.Scopes, time.Duration(req.Msg.LifetimeSeconds)*time.Second, req.Msg.ServicePrincipalId)
	if err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("credential issuance denied; check project, scopes and lifetime, or revoke an active credential if the 100-per-project limit was reached"))
	}
	return connect.NewResponse(&api.CreateApiTokenResponse{Credential: apiTokenMessage(token), Token: raw}), nil
}
func (s *workflowService) ListApiTokens(ctx context.Context, req *connect.Request[api.ListApiTokensRequest]) (*connect.Response[api.ListApiTokensResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	tokens, err := s.sessions.ListAPITokens(ctx, caller, req.Msg.ProjectId, req.Msg.ServicePrincipalId)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("credential inventory unavailable"))
	}
	out := &api.ListApiTokensResponse{}
	for _, token := range tokens {
		out.Credentials = append(out.Credentials, apiTokenMessage(token))
	}
	return connect.NewResponse(out), nil
}
func (s *workflowService) RevokeApiToken(ctx context.Context, req *connect.Request[api.RevokeApiTokenRequest]) (*connect.Response[api.RevokeApiTokenResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err = s.sessions.RevokeAPIToken(ctx, caller, req.Msg.TokenId); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("credential revocation denied"))
	}
	return connect.NewResponse(&api.RevokeApiTokenResponse{}), nil
}
