package main

import (
	"connectrpc.com/connect"
	"context"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"time"
)

func checkpointMessage(c workflow.GoalCheckpoint) *api.GoalCheckpoint {
	return &api.GoalCheckpoint{Id: c.ID, GoalId: c.GoalID, RunId: c.RunID, PackageId: c.PackageID, CandidateRevision: c.CandidateRevision, Title: c.Title, PrincipalId: c.PrincipalID, Sequence: c.Sequence, PlanVersion: c.PlanVersion, GoalRevision: c.GoalRevision, CreatedAt: c.CreatedAt.UTC().Format(time.RFC3339Nano), CurrentAcceptance: c.CurrentAcceptance}
}
func (s *workflowService) ListGoalCheckpoints(ctx context.Context, req *connect.Request[api.ListGoalCheckpointsRequest]) (*connect.Response[api.ListGoalCheckpointsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	list, more, err := s.store.ListGoalCheckpoints(ctx, caller.OrganizationID, req.Msg.GoalId, req.Msg.BeforeSequence)
	if err != nil {
		return nil, workflowError(err)
	}
	result := &api.ListGoalCheckpointsResponse{}
	for _, c := range list {
		result.Checkpoints = append(result.Checkpoints, checkpointMessage(c))
	}
	if more {
		result.NextBeforeSequence = list[len(list)-1].Sequence
	}
	return connect.NewResponse(result), nil
}
func (s *workflowService) RecordGoalCheckpoint(ctx context.Context, req *connect.Request[api.RecordGoalCheckpointRequest]) (*connect.Response[api.RecordGoalCheckpointResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	in := req.Msg
	c, err := s.store.RecordGoalCheckpoint(ctx, caller, in.GoalId, in.RunId, in.PackageId, in.Title, in.RequestKey, in.ExpectedGoalRevision)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.RecordGoalCheckpointResponse{Checkpoint: checkpointMessage(c)}), nil
}
