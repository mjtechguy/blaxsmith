package main

import (
	"connectrpc.com/connect"
	"context"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
)

func (s *workflowService) GetProjectModelOptions(ctx context.Context, req *connect.Request[api.GetProjectModelOptionsRequest]) (*connect.Response[api.GetProjectModelOptionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	options, err := s.store.ProjectModelOptions(ctx, caller, req.Msg.ProjectId, req.Msg.Harness)
	if err != nil {
		return nil, workflowError(err)
	}
	out := &api.GetProjectModelOptionsResponse{}
	for _, option := range options {
		item := &api.ProjectModelOption{ConnectionId: option.ConnectionID, Label: option.Label, Provider: option.Provider, OwnerKind: option.OwnerKind, AuthMethod: option.AuthMethod, CheckedAt: optionalTime(option.CheckedAt), CatalogError: option.CatalogError}
		for _, model := range option.Models {
			item.Models = append(item.Models, connectionModelMessage(model))
		}
		out.Connections = append(out.Connections, item)
	}
	return connect.NewResponse(out), nil
}
