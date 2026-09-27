package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func (s *workflowService) GetGoalAllowance(ctx context.Context, req *connect.Request[api.GetGoalAllowanceRequest]) (*connect.Response[api.GetGoalAllowanceResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	value, err := s.store.GetGoalAllowance(ctx, caller.OrganizationID, req.Msg.GoalId)
	if err != nil {
		return nil, workflowError(err)
	}
	out := &api.GoalAllowance{Version: value.Version, MaxRuns: int32(value.MaxRuns), MaxAttempts: int32(value.MaxAttempts), Runs: value.Runs, Attempts: value.Attempts, AdmissionClosed: value.AdmissionClosed, PrincipalId: value.PrincipalID, MaxRepeatedCheckFailures: int32(value.MaxRepeatedCheckFailures), NoProgressSeconds: int32(value.NoProgressSeconds), RepeatedCheckFailures: value.RepeatedCheckFailures, StallReason: value.StallReason}
	if value.AdmitUntil != nil {
		out.AdmitUntil = value.AdmitUntil.Format(time.RFC3339)
	}
	if value.StallResetAt != nil {
		out.StallResetAt = value.StallResetAt.UTC().Format(time.RFC3339Nano)
	}
	if value.LastProgressAt != nil {
		out.LastProgressAt = value.LastProgressAt.UTC().Format(time.RFC3339Nano)
	}
	if value.UpdatedAt != nil {
		out.UpdatedAt = value.UpdatedAt.Format(time.RFC3339)
	}
	return connect.NewResponse(&api.GetGoalAllowanceResponse{Allowance: out}), nil
}
func (s *workflowService) SetGoalAllowance(ctx context.Context, req *connect.Request[api.SetGoalAllowanceRequest]) (*connect.Response[api.SetGoalAllowanceResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	var deadline *time.Time
	if req.Msg.AdmitUntil != "" {
		value, err := time.Parse(time.RFC3339, req.Msg.AdmitUntil)
		if err != nil {
			return nil, workflowError(workflow.ErrInvalid)
		}
		deadline = &value
	}
	err = s.interactions.SetGoalAllowance(ctx, caller, req.Msg.GoalId, req.Msg.ExpectedVersion, int(req.Msg.MaxRuns), int(req.Msg.MaxAttempts), deadline, int(req.Msg.MaxRepeatedCheckFailures), int(req.Msg.NoProgressSeconds), req.Msg.ResetStallWindow)
	if errors.Is(err, workflow.ErrConflict) {
		return nil, connect.NewError(connect.CodeAborted, errors.New("goal allowance changed; reload before saving"))
	}
	if err != nil {
		return nil, goalError(err)
	}
	return connect.NewResponse(&api.SetGoalAllowanceResponse{}), nil
}
