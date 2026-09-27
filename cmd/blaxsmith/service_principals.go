package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func servicePrincipalMessage(p identity.ServicePrincipal) *api.ServicePrincipal {
	return &api.ServicePrincipal{Id: p.ID, ProjectId: p.ProjectID, Label: p.Label, State: p.State, CreatedBy: p.CreatedBy, CreatedAt: p.CreatedAt.Format(time.RFC3339)}
}
func (s *workflowService) CreateServicePrincipal(ctx context.Context, req *connect.Request[api.CreateServicePrincipalRequest]) (*connect.Response[api.CreateServicePrincipalResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	principal, err := s.sessions.CreateServicePrincipal(ctx, caller, req.Msg.ProjectId, req.Msg.Label, req.Msg.RequestKey)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("service identity creation denied; requires an organization administrator, a valid project and a stable request key"))
	}
	return connect.NewResponse(&api.CreateServicePrincipalResponse{Principal: servicePrincipalMessage(principal)}), nil
}
func (s *workflowService) ListServicePrincipals(ctx context.Context, req *connect.Request[api.ListServicePrincipalsRequest]) (*connect.Response[api.ListServicePrincipalsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	principals, err := s.sessions.ListServicePrincipals(ctx, caller, req.Msg.ProjectId)
	if err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("service identity inventory requires an organization administrator"))
	}
	out := &api.ListServicePrincipalsResponse{}
	for _, p := range principals {
		out.Principals = append(out.Principals, servicePrincipalMessage(p))
	}
	return connect.NewResponse(out), nil
}
func (s *workflowService) DisableServicePrincipal(ctx context.Context, req *connect.Request[api.DisableServicePrincipalRequest]) (*connect.Response[api.DisableServicePrincipalResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err = s.sessions.DisableServicePrincipal(ctx, caller, req.Msg.PrincipalId); err != nil {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("service identity disable denied"))
	}
	return connect.NewResponse(&api.DisableServicePrincipalResponse{}), nil
}
