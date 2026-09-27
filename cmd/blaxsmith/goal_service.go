package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/anvil"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func (s *workflowService) CreateGoal(ctx context.Context, req *connect.Request[api.CreateGoalRequest]) (*connect.Response[api.CreateGoalResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	factoryID, factoryVersion := strings.TrimSpace(req.Msg.FactoryId), strings.TrimSpace(req.Msg.FactoryVersion)
	var questions []interact.Interaction
	if factoryID == "" {
		factoryID = "anvil"
	}
	if factoryID == "anvil" {
		if len(req.Msg.Questions) > 0 || (factoryVersion != "" && factoryVersion != anvil.Version) {
			return nil, workflowError(workflow.ErrInvalid)
		}
		factoryVersion = anvil.Version
		questions = anvil.StarterQuestions()
	} else {
		for _, q := range req.Msg.Questions {
			question := interact.Interaction{ID: q.Id, Kind: "question", Title: q.Title, BodyMD: q.BodyMd, MultiSelect: q.MultiSelect, AllowFreeText: q.AllowFreeText, Blocking: q.Blocking}
			for _, option := range q.Options {
				question.Options = append(question.Options, interact.Option{ID: option.Id, Label: option.Label, Description: option.Description, Recommended: option.Recommended})
			}
			questions = append(questions, question)
		}
	}
	id, err := s.interactions.CreateGoal(ctx, caller, interact.GoalInput{ProjectID: req.Msg.ProjectId, RequestKey: req.Msg.RequestKey, Title: req.Msg.Title, Brief: req.Msg.Brief, FactoryID: factoryID, FactoryVersion: factoryVersion, Questions: questions})
	if err != nil {
		return nil, goalError(err)
	}
	g, _, _, err := s.interactions.GetGoal(ctx, caller.OrganizationID, id, 0)
	if err != nil {
		return nil, goalError(err)
	}
	return connect.NewResponse(&api.CreateGoalResponse{Goal: goalMessage(g)}), nil
}
func (s *workflowService) ListGoals(ctx context.Context, req *connect.Request[api.ListGoalsRequest]) (*connect.Response[api.ListGoalsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err = s.store.GetProject(ctx, caller.OrganizationID, req.Msg.ProjectId); err != nil {
		return nil, goalError(err)
	}
	goals, more, err := s.interactions.ListGoals(ctx, caller.OrganizationID, req.Msg.ProjectId, req.Msg.BeforeId)
	if err != nil {
		return nil, goalError(err)
	}
	out := &api.ListGoalsResponse{}
	for _, g := range goals {
		out.Goals = append(out.Goals, goalMessage(g))
	}
	if more {
		out.NextBeforeId = goals[len(goals)-1].ID
	}
	return connect.NewResponse(out), nil
}
func (s *workflowService) GetGoal(ctx context.Context, req *connect.Request[api.GetGoalRequest]) (*connect.Response[api.GetGoalResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	g, entries, more, err := s.interactions.GetGoal(ctx, caller.OrganizationID, req.Msg.GoalId, req.Msg.BeforeSequence)
	if err != nil {
		return nil, goalError(err)
	}
	out := &api.GetGoalResponse{Goal: goalMessage(g)}
	for _, e := range entries {
		out.Entries = append(out.Entries, &api.GoalEntry{Sequence: e.Sequence, Kind: e.Kind, QuestionId: e.QuestionID, OptionIds: e.OptionIDs, Text: e.Text, PrincipalId: e.PrincipalID, CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339Nano)})
	}
	if more {
		out.NextBeforeSequence = entries[0].Sequence
	}
	return connect.NewResponse(out), nil
}
func (s *workflowService) ReplyGoal(ctx context.Context, req *connect.Request[api.ReplyGoalRequest]) (*connect.Response[api.ReplyGoalResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	err = s.interactions.ReplyGoal(ctx, caller, interact.GoalReply{GoalID: req.Msg.GoalId, RequestKey: req.Msg.RequestKey, ExpectedRevision: req.Msg.ExpectedRevision, Kind: req.Msg.Kind, QuestionID: req.Msg.QuestionId, OptionIDs: req.Msg.OptionIds, Text: req.Msg.Text})
	if err != nil {
		return nil, goalError(err)
	}
	return connect.NewResponse(&api.ReplyGoalResponse{}), nil
}
func goalMessage(g interact.Goal) *api.Goal {
	out := &api.Goal{Id: g.ID, ProjectId: g.ProjectID, Title: g.Title, Brief: g.Brief, FactoryId: g.FactoryID, FactoryVersion: g.FactoryVersion, Revision: g.Revision, CreatedBy: g.CreatedBy, CreatedAt: g.CreatedAt.UTC().Format(time.RFC3339Nano), UpdatedAt: g.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	for _, q := range g.Questions {
		out.Questions = append(out.Questions, interactionMessage(q))
	}
	return out
}
func goalError(err error) error {
	if errors.Is(err, interact.ErrDenied) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("goal change denied"))
	}
	return workflowError(err)
}
