package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// gatewayAdminService serves Admin → Settings → Model gateway and Admin →
// Usage & Gateway (docs/model-gateway-plan.md §9.1, §15.1).
type gatewayAdminService struct {
	guard *identity.BrowserGuard
	store *workflow.Store
	// installed reports whether this installation runs the gateway
	// Deployment (BLAXSMITH_GATEWAY_URL set by the Helm chart).
	installed bool
}

// usageService serves any member's usage, the run Cost tab, and the
// project delivery-mode setting.
type usageService struct {
	guard *identity.BrowserGuard
	store *workflow.Store
}

func gatewayError(err error) error {
	switch {
	case errors.Is(err, workflow.ErrGatewayUnavailable):
		return connect.NewError(connect.CodeFailedPrecondition, errors.New("this installation does not run the model gateway (Helm gateway.enabled)"))
	case errors.Is(err, workflow.ErrConflict):
		return connect.NewError(connect.CodeAborted, errors.New("settings changed since you loaded them; reload and try again"))
	case errors.Is(err, workflow.ErrConnectionDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("project administration denied, or the organization enforces its default delivery mode"))
	}
	return adminError(err)
}

func settingsMessage(s workflow.GatewaySettings) *api.GatewaySettings {
	return &api.GatewaySettings{Enabled: s.Enabled, DefaultDeliveryMode: s.DefaultDeliveryMode,
		AllowProjectChoice: s.AllowProjectChoice, RemoveDirectEgress: s.RemoveDirectEgress}
}

func totalsMessage(t workflow.UsageTotals) *api.UsageTotals {
	return &api.UsageTotals{Requests: t.Requests, Errors: t.Errors, RateLimited: t.RateLimited, InputTokens: t.Input,
		OutputTokens: t.Output, CacheReadTokens: t.CacheRead, CacheWriteTokens: t.CacheWrite, ReasoningTokens: t.Reasoning,
		CostUsdMicros: t.CostUSDMicro}
}

func slicesMessage(items []workflow.UsageSlice) []*api.UsageSlice {
	out := make([]*api.UsageSlice, 0, len(items))
	for _, item := range items {
		out = append(out, &api.UsageSlice{Key: item.Key, Label: item.Label, Detail: item.Detail, ProjectId: item.ProjectID, Totals: totalsMessage(item.Totals)})
	}
	return out
}

func (s *gatewayAdminService) GetGatewaySettings(ctx context.Context, req *connect.Request[api.GetGatewaySettingsRequest]) (*connect.Response[api.GetGatewaySettingsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	view, err := s.store.GatewaySettingsAs(ctx, caller)
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.GetGatewaySettingsResponse{Settings: settingsMessage(view.Settings),
		InstallationAvailable: s.installed, Version: view.Version, UpdatedAt: adminOptionalTime(view.UpdatedAt),
		UpdatedByUsername: view.UpdatedByUsername}), nil
}

func (s *gatewayAdminService) UpdateGatewaySettings(ctx context.Context, req *connect.Request[api.UpdateGatewaySettingsRequest]) (*connect.Response[api.UpdateGatewaySettingsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	in := req.Msg.GetSettings()
	if in == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("settings are required"))
	}
	view, err := s.store.UpdateGatewaySettingsAs(ctx, caller, workflow.GatewaySettings{Enabled: in.Enabled,
		DefaultDeliveryMode: in.DefaultDeliveryMode, AllowProjectChoice: in.AllowProjectChoice,
		RemoveDirectEgress: in.RemoveDirectEgress}, req.Msg.ExpectedVersion, s.installed)
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.UpdateGatewaySettingsResponse{Settings: settingsMessage(view.Settings), Version: view.Version}), nil
}

func (s *gatewayAdminService) GetUsageOverview(ctx context.Context, req *connect.Request[api.GetUsageOverviewRequest]) (*connect.Response[api.GetUsageOverviewResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if req.Msg.Days < 0 || req.Msg.Days > 90 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("days must be 1–90"))
	}
	o, err := s.store.UsageOverviewAs(ctx, caller, int(req.Msg.Days), req.Msg.SeriesBy)
	if err != nil {
		return nil, gatewayError(err)
	}
	response := &api.GetUsageOverviewResponse{Enabled: o.Enabled, FromDay: o.From.Format("2006-01-02"), ToDay: o.To.Format("2006-01-02"),
		Totals: totalsMessage(o.Totals), MedianTtftMs: o.MedianTTFTMS, SeriesLabels: slicesMessage(o.SeriesLabels),
		TopProjects: slicesMessage(o.TopProjects), TopUsers: slicesMessage(o.TopUsers), TopRuns: slicesMessage(o.TopRuns)}
	for _, p := range o.Series {
		response.Series = append(response.Series, &api.UsagePoint{Day: p.Day, Key: p.Key, CostUsdMicros: p.Cost, Tokens: p.Tokens})
	}
	return connect.NewResponse(response), nil
}

func priceMessage(p workflow.ModelPrice) *api.ModelPrice {
	return &api.ModelPrice{Provider: p.Provider, Model: p.Model, InputMicrosPerMtok: p.Input, OutputMicrosPerMtok: p.Output,
		CacheReadMicrosPerMtok: p.CacheRead, CacheWriteMicrosPerMtok: p.CacheWrite, Source: p.Source, Version: p.Version,
		EffectiveFrom: adminOptionalTime(p.EffectiveFrom)}
}

func (s *gatewayAdminService) ListModelPrices(ctx context.Context, req *connect.Request[api.ListModelPricesRequest]) (*connect.Response[api.ListModelPricesResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	prices, err := s.store.ModelPricesAs(ctx, caller)
	if err != nil {
		return nil, gatewayError(err)
	}
	response := &api.ListModelPricesResponse{}
	for _, p := range prices {
		response.Prices = append(response.Prices, priceMessage(p))
	}
	return connect.NewResponse(response), nil
}

func (s *gatewayAdminService) SetModelPriceOverride(ctx context.Context, req *connect.Request[api.SetModelPriceOverrideRequest]) (*connect.Response[api.SetModelPriceOverrideResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	in := req.Msg.GetPrice()
	if in == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("price is required"))
	}
	p, err := s.store.SetModelPriceOverrideAs(ctx, caller, workflow.ModelPrice{Provider: in.Provider, Model: in.Model,
		Input: in.InputMicrosPerMtok, Output: in.OutputMicrosPerMtok, CacheRead: in.CacheReadMicrosPerMtok,
		CacheWrite: in.CacheWriteMicrosPerMtok})
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.SetModelPriceOverrideResponse{Price: priceMessage(p)}), nil
}

func (s *usageService) GetGatewayStatus(ctx context.Context, req *connect.Request[api.GetGatewayStatusRequest]) (*connect.Response[api.GetGatewayStatusResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	enabled, err := s.store.GatewayEnabled(ctx, caller.OrganizationID)
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.GetGatewayStatusResponse{Enabled: enabled}), nil
}

func (s *usageService) GetMyUsage(ctx context.Context, req *connect.Request[api.GetMyUsageRequest]) (*connect.Response[api.GetMyUsageResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	if req.Msg.Days < 0 || req.Msg.Days > 90 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("days must be 1–90"))
	}
	u, err := s.store.MyUsageAs(ctx, caller, int(req.Msg.Days))
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.GetMyUsageResponse{Enabled: u.Enabled, FromDay: u.From.Format("2006-01-02"),
		ToDay: u.To.Format("2006-01-02"), Totals: totalsMessage(u.Totals), ByProject: slicesMessage(u.ByProject),
		ByModel: slicesMessage(u.ByModel), TopRuns: slicesMessage(u.TopRuns)}), nil
}

func (s *usageService) GetRunCost(ctx context.Context, req *connect.Request[api.GetRunCostRequest]) (*connect.Response[api.GetRunCostResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	cost, err := s.store.RunCostAs(ctx, caller, req.Msg.RunId)
	if err != nil {
		return nil, gatewayError(err)
	}
	response := &api.GetRunCostResponse{Enabled: cost.Enabled, Totals: totalsMessage(cost.Totals), Truncated: cost.Truncated,
		RequestsRestricted: cost.CallsRestricted}
	for _, stage := range cost.Stages {
		ratio := 0.0
		if in := stage.Totals.Input + stage.Totals.CacheRead + stage.Totals.CacheWrite; in > 0 {
			ratio = float64(stage.Totals.CacheRead) / float64(in)
		}
		response.Stages = append(response.Stages, &api.StageCost{TaskId: stage.TaskID, Stage: stage.Stage,
			Totals: totalsMessage(stage.Totals), CacheHitRatio: ratio})
	}
	for _, c := range cost.Calls {
		response.Requests = append(response.Requests, &api.ModelCall{StartedAt: adminTime(c.StartedAt), Stage: c.Stage,
			Model: c.Model, RouteKind: c.RouteKind, Api: c.API, Status: c.Status, HttpStatus: c.HTTPStatus,
			RetryCount: c.RetryCount, Streamed: c.Streamed, UsageReported: c.UsageReported, TtftMs: c.TTFTMS,
			DurationMs: c.DurationMS, Totals: totalsMessage(c.Totals)})
	}
	return connect.NewResponse(response), nil
}

func deliveryMessage(d workflow.ProjectDelivery) *api.GetProjectDeliveryResponse {
	return &api.GetProjectDeliveryResponse{DeliveryMode: d.Effective, ProjectChoice: d.ProjectChoice, OrgDefault: d.OrgDefault,
		GatewayEnabled: d.GatewayEnabled, ChoiceAllowed: d.ChoiceAllowed, CanEdit: d.CanEdit}
}

func (s *usageService) GetProjectDelivery(ctx context.Context, req *connect.Request[api.GetProjectDeliveryRequest]) (*connect.Response[api.GetProjectDeliveryResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	d, err := s.store.ProjectDeliveryAs(ctx, caller, req.Msg.ProjectId)
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(deliveryMessage(d)), nil
}

func (s *usageService) SetProjectDelivery(ctx context.Context, req *connect.Request[api.SetProjectDeliveryRequest]) (*connect.Response[api.SetProjectDeliveryResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	d, err := s.store.SetProjectDeliveryAs(ctx, caller, req.Msg.ProjectId, req.Msg.DeliveryMode)
	if err != nil {
		return nil, gatewayError(err)
	}
	return connect.NewResponse(&api.SetProjectDeliveryResponse{Delivery: deliveryMessage(d)}), nil
}
