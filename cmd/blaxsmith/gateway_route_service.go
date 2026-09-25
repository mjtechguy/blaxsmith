package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/gateway"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// Admin → Routes & pools (docs/model-gateway-plan.md §4, §5, §9.1) and the
// owner's subscription meters (§6, §9.2). Credentials are write-only.

func routeMessage(r workflow.GatewayRoute) *api.GatewayRoute {
	out := &api.GatewayRoute{Id: r.ID, Name: r.Name, Kind: r.Kind, ConnectionId: r.ConnectionID, ConnectionLabel: r.ConnectionLabel,
		Region: r.Region, CloudProject: r.CloudProject, ModelMap: r.ModelMap, Weight: int32(r.Weight), Priority: int32(r.Priority),
		ConcurrencyCap: int32(r.Cap), RequestsPerMinute: int32(r.RequestsPerMinute), TokensPerMinute: r.TokensPerMinute,
		State: r.State, PoolIds: r.PoolIDs, Breaker: r.Breaker, CooldownUntil: adminOptionalTime(r.CooldownUntil),
		Inflight: int32(r.Inflight), Requests_15M: int32(r.Requests15m), Errors_15M: int32(r.Errors15m),
		Last_429At: adminOptionalTime(r.Last429), StateUpdatedAt: adminOptionalTime(r.StateUpdatedAt)}
	for _, m := range r.Metrics {
		out.Metrics = append(out.Metrics, &api.GatewayRouteMetric{Name: m.Name, Limit: m.Limit, Remaining: m.Remaining,
			ResetAt: adminOptionalTime(m.ResetAt)})
	}
	return out
}

func poolMessage(p workflow.GatewayPool) *api.GatewayPool {
	return &api.GatewayPool{Id: p.ID, Name: p.Name, Family: p.Family, Strategy: p.Strategy, ConcurrencyCap: int32(p.Cap),
		Affinity: p.Affinity, State: p.State, RouteIds: p.RouteIDs, ProjectIds: p.ProjectIDs}
}

func optionMessages(items []workflow.GatewayOption) []*api.GatewayRouteOption {
	out := make([]*api.GatewayRouteOption, 0, len(items))
	for _, o := range items {
		out = append(out, &api.GatewayRouteOption{Id: o.ID, Label: o.Label, Detail: o.Detail})
	}
	return out
}

func (s *gatewayAdminService) ListGatewayRoutes(ctx context.Context, req *connect.Request[api.ListGatewayRoutesRequest]) (*connect.Response[api.ListGatewayRoutesResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	view, err := s.store.GatewayRoutesAs(ctx, caller)
	if err != nil {
		return nil, gatewayError(err)
	}
	response := &api.ListGatewayRoutesResponse{Connections: optionMessages(view.Connections), Projects: optionMessages(view.Projects),
		PoolsEnabled: view.PoolsEnabled, PacingEnabled: view.PacingEnabled}
	for _, r := range view.Routes {
		response.Routes = append(response.Routes, routeMessage(r))
	}
	for _, p := range view.Pools {
		response.Pools = append(response.Pools, poolMessage(p))
	}
	for _, p := range view.Paced {
		response.Paced = append(response.Paced, &api.PacedStage{TaskId: p.TaskID, RunId: p.RunID, ProjectId: p.ProjectID,
			Stage: p.Stage, PoolId: p.PoolID, Reason: p.Reason, ResetsAt: adminOptionalTime(p.ResetsAt), Since: adminTime(p.Since)})
	}
	return connect.NewResponse(response), nil
}

// cloudCredential validates a Bedrock or Vertex credential's shape before it
// is sealed; the parsed form is discarded.
func cloudCredential(kind, raw string) (*workflow.CloudCredential, error) {
	if raw == "" {
		return nil, nil
	}
	invalid := connect.NewError(connect.CodeInvalidArgument, errors.New("the cloud credential is not valid for this route kind"))
	switch kind {
	case gateway.KindBedrock:
		c, err := gateway.ParseAWSCredential([]byte(raw))
		if err != nil {
			return nil, invalid
		}
		return &workflow.CloudCredential{Secret: []byte(raw), ExternalAccount: c.AccessKeyID}, nil
	case gateway.KindVertex:
		sa, err := gateway.ParseServiceAccount([]byte(raw))
		if err != nil {
			return nil, invalid
		}
		return &workflow.CloudCredential{Secret: []byte(raw), ExternalAccount: sa.ClientEmail}, nil
	}
	return nil, invalid
}

func (s *gatewayAdminService) SaveGatewayRoute(ctx context.Context, req *connect.Request[api.SaveGatewayRouteRequest]) (*connect.Response[api.SaveGatewayRouteResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	in := req.Msg.GetRoute()
	if in == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("route is required"))
	}
	kind := in.Kind
	if in.Id != "" && req.Msg.CloudCredential != "" {
		// Rotation: the stored kind decides the credential shape.
		view, err := s.store.GatewayRoutesAs(ctx, caller)
		if err != nil {
			return nil, gatewayError(err)
		}
		for _, r := range view.Routes {
			if r.ID == in.Id {
				kind = r.Kind
			}
		}
	}
	credential, err := cloudCredential(kind, req.Msg.CloudCredential)
	if err != nil {
		return nil, err
	}
	if credential != nil {
		defer clear(credential.Secret)
	}
	route, err := s.store.SaveGatewayRouteAs(ctx, caller, workflow.GatewayRoute{ID: in.Id, Name: in.Name, Kind: in.Kind,
		ConnectionID: in.ConnectionId, Region: in.Region, CloudProject: in.CloudProject, ModelMap: in.ModelMap,
		Weight: int(in.Weight), Priority: int(in.Priority), Cap: int(in.ConcurrencyCap), RequestsPerMinute: int(in.RequestsPerMinute),
		TokensPerMinute: in.TokensPerMinute, State: in.State}, credential, s.secrets)
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.SaveGatewayRouteResponse{Route: routeMessage(route)}), nil
}

func (s *gatewayAdminService) SetGatewayRouteState(ctx context.Context, req *connect.Request[api.SetGatewayRouteStateRequest]) (*connect.Response[api.SetGatewayRouteStateResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetGatewayRouteStateAs(ctx, caller, req.Msg.Id, req.Msg.State); err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.SetGatewayRouteStateResponse{}), nil
}

func (s *gatewayAdminService) SaveGatewayPool(ctx context.Context, req *connect.Request[api.SaveGatewayPoolRequest]) (*connect.Response[api.SaveGatewayPoolResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	in := req.Msg.GetPool()
	if in == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("pool is required"))
	}
	pool, err := s.store.SaveGatewayPoolAs(ctx, caller, workflow.GatewayPool{ID: in.Id, Name: in.Name, Family: in.Family,
		Strategy: in.Strategy, Cap: int(in.ConcurrencyCap), Affinity: in.Affinity, State: in.State, RouteIDs: in.RouteIds,
		ProjectIDs: in.ProjectIds})
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.SaveGatewayPoolResponse{Pool: poolMessage(pool)}), nil
}

func (s *usageService) ListMySubscriptionLimits(ctx context.Context, req *connect.Request[api.ListMySubscriptionLimitsRequest]) (*connect.Response[api.ListMySubscriptionLimitsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	items, personal, err := s.store.MySubscriptionLimitsAs(ctx, caller)
	if err != nil {
		return nil, gatewayError(err)
	}
	response := &api.ListMySubscriptionLimitsResponse{PersonalRoutesEnabled: personal}
	for _, item := range items {
		out := &api.SubscriptionLimits{ConnectionId: item.ConnectionID, Label: item.Label, Provider: item.Provider,
			AuthMethod: item.AuthMethod, State: item.State}
		for _, w := range item.Windows {
			out.Windows = append(out.Windows, &api.SubscriptionLimitWindow{Name: w.Name, UsedPct: w.UsedPct,
				WindowMinutes: int32(w.WindowMinutes), ResetsAt: adminOptionalTime(w.ResetsAt), ObservedAt: adminTime(w.ObservedAt)})
		}
		response.Subscriptions = append(response.Subscriptions, out)
	}
	return connect.NewResponse(response), nil
}
