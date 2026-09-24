package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/interact"
)

func (s *workflowService) ListInteractions(ctx context.Context, req *connect.Request[api.ListInteractionsRequest]) (*connect.Response[api.ListInteractionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if _, err := s.store.GetRun(ctx, caller.OrganizationID, req.Msg.RunId); err != nil {
		return nil, workflowError(err)
	}
	if s.interactions == nil {
		return connect.NewResponse(&api.ListInteractionsResponse{}), nil
	}
	records, err := s.interactions.ListInteractions(ctx, caller.OrganizationID, req.Msg.RunId)
	if err != nil {
		return nil, workflowError(err)
	}
	response := &api.ListInteractionsResponse{}
	for _, record := range records {
		response.Interactions = append(response.Interactions, interactionMessage(record))
	}
	return connect.NewResponse(response), nil
}

// AnswerInteraction and SteerAttempt require a CSRF-checked mutation session;
// the store rechecks the live session and run-control role under lock.
func (s *workflowService) AnswerInteraction(ctx context.Context, req *connect.Request[api.AnswerInteractionRequest]) (*connect.Response[api.AnswerInteractionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if s.interactions == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("interactions are not configured"))
	}
	record, err := s.interactions.Answer(ctx, caller, req.Msg.InteractionId, req.Msg.OptionIds, req.Msg.Text)
	if err != nil {
		return nil, interactionError(err)
	}
	return connect.NewResponse(&api.AnswerInteractionResponse{Interaction: interactionMessage(record)}), nil
}

func (s *workflowService) SteerAttempt(ctx context.Context, req *connect.Request[api.SteerAttemptRequest]) (*connect.Response[api.SteerAttemptResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if s.interactions == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("interactions are not configured"))
	}
	id, err := s.interactions.Steer(ctx, caller, req.Msg.AttemptId, req.Msg.Kind, req.Msg.Text, req.Msg.Reason, req.Msg.N)
	if err != nil {
		return nil, interactionError(err)
	}
	return connect.NewResponse(&api.SteerAttemptResponse{SteerId: id}), nil
}

func interactionError(err error) error {
	if errors.Is(err, interact.ErrDenied) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("run control denied"))
	}
	return workflowError(err)
}

func interactionMessage(r interact.Record) *api.Interaction {
	message := &api.Interaction{Id: r.ID, AttemptId: r.AttemptID, Stage: r.Stage, Kind: r.Kind, Title: r.Title,
		BodyMd: r.BodyMD, MultiSelect: r.MultiSelect, AllowFreeText: r.AllowFreeText, Blocking: r.Blocking,
		State: r.State, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339Nano)}
	for _, o := range r.Options {
		message.Options = append(message.Options, &api.InteractionOption{Id: o.ID, Label: o.Label,
			Description: o.Description, Recommended: o.Recommended})
	}
	for _, source := range r.Sources {
		message.Sources = append(message.Sources, &api.InteractionSource{Path: source.Path, Line: int32(source.Line)})
	}
	if r.Interview != nil {
		message.Interview = &api.InteractionInterview{Round: int32(r.Interview.Round), FinalizeOption: r.Interview.FinalizeOption}
	}
	if r.Answer != nil {
		message.Answer = &api.InteractionAnswer{OptionIds: r.Answer.OptionIDs, Text: r.Answer.Text,
			AnsweredBy: r.Answer.AnsweredBy, At: r.Answer.At.UTC().Format(time.RFC3339Nano)}
	}
	return message
}
