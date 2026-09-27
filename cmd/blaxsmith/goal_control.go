package main

import (
	"connectrpc.com/connect"
	"context"
	"errors"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func (s *workflowService) GetGoalControl(ctx context.Context, req *connect.Request[api.GetGoalControlRequest]) (*connect.Response[api.GetGoalControlResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	value, err := s.store.GetGoalControl(ctx, caller.OrganizationID, req.Msg.GoalId)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetGoalControlResponse{Control: &api.GoalControl{State: value.State, Version: value.Version, ActiveRuns: value.ActiveRuns, StoppingRuns: value.StoppingRuns}}), nil
}
func (s *workflowService) ControlGoal(ctx context.Context, req *connect.Request[api.ControlGoalRequest]) (*connect.Response[api.ControlGoalResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	version, err := s.store.ControlGoal(ctx, caller, req.Msg.GoalId, req.Msg.Action, req.Msg.RequestKey, req.Msg.ExpectedVersion)
	if errors.Is(err, workflow.ErrConflict) {
		return nil, connect.NewError(connect.CodeAborted, errors.New("goal control changed or action no longer applies; refresh before retrying"))
	}
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.ControlGoalResponse{Version: version}), nil
}
