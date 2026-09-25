package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// Model gateway G3 (docs/model-gateway-plan.md §8, §9): Admin → Budgets and
// Admin → Alerts, Project → Usage, and alert acknowledge/snooze for recipients.

func budgetError(err error) error {
	switch {
	case errors.Is(err, workflow.ErrBudgetExists):
		return connect.NewError(connect.CodeAlreadyExists, workflow.ErrBudgetExists)
	case errors.Is(err, workflow.ErrConflict):
		return connect.NewError(connect.CodeAborted, errors.New("the budget changed since you loaded it; reload and try again"))
	}
	return gatewayError(err)
}

const day = "2006-01-02"

func budgetMessage(b workflow.Budget) *api.Budget {
	return &api.Budget{Id: b.ID, Name: b.Name, Scope: b.Scope, ProjectId: b.ProjectID, ProjectName: b.ProjectName,
		PrincipalId: b.PrincipalID, PrincipalName: b.PrincipalName, AmountUsdMicros: b.AmountUSDMicros, Thresholds: b.Thresholds,
		Version: b.Version, SpendUsdMicros: b.SpendUSDMicros, ForecastUsdMicros: b.ForecastUSDMicros, FiredThresholds: b.FiredThresholds,
		PeriodStart: b.PeriodStart.Format(day), PeriodEnd: b.PeriodEnd.AddDate(0, 0, -1).Format(day), CreatedAt: adminTime(b.CreatedAt)}
}

func alertMessages(alerts []workflow.BudgetAlert) []*api.BudgetAlert {
	out := make([]*api.BudgetAlert, 0, len(alerts))
	for _, a := range alerts {
		out = append(out, &api.BudgetAlert{Id: a.ID, BudgetId: a.BudgetID, BudgetName: a.BudgetName, Scope: a.Scope,
			ProjectId: a.ProjectID, ProjectName: a.ProjectName, PrincipalId: a.PrincipalID, PrincipalName: a.PrincipalName,
			ThresholdPct: a.ThresholdPct, SpendUsdMicros: a.SpendUSDMicros, AmountUsdMicros: a.AmountUSDMicros,
			ForecastUsdMicros: a.ForecastUSDMicros, PeriodStart: a.PeriodStart.Format(day), CreatedAt: adminTime(a.CreatedAt),
			AcknowledgedAt: adminOptionalTime(a.AcknowledgedAt), AcknowledgedByUsername: a.AcknowledgedBy,
			SnoozedUntil: adminOptionalTime(a.SnoozedUntil)})
	}
	return out
}

func budgetInput(in *api.BudgetInput) workflow.BudgetInput {
	return workflow.BudgetInput{Name: in.GetName(), Scope: in.GetScope(), ProjectID: in.GetProjectId(),
		PrincipalID: in.GetPrincipalId(), AmountUSDMicros: in.GetAmountUsdMicros(), Thresholds: in.GetThresholds()}
}

func (s *gatewayAdminService) ListBudgets(ctx context.Context, req *connect.Request[api.ListBudgetsRequest]) (*connect.Response[api.ListBudgetsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	view, err := s.store.BudgetsAs(ctx, caller)
	if err != nil {
		return nil, budgetError(err)
	}
	response := &api.ListBudgetsResponse{GatewayEnabled: view.GatewayEnabled, BudgetsEnabled: view.BudgetsEnabled}
	for _, b := range view.Budgets {
		response.Budgets = append(response.Budgets, budgetMessage(b))
	}
	return connect.NewResponse(response), nil
}

func (s *gatewayAdminService) SetBudgetsEnabled(ctx context.Context, req *connect.Request[api.SetBudgetsEnabledRequest]) (*connect.Response[api.SetBudgetsEnabledResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	enabled, err := s.store.SetBudgetsEnabledAs(ctx, caller, req.Msg.Enabled)
	if err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.SetBudgetsEnabledResponse{Enabled: enabled}), nil
}

func (s *gatewayAdminService) CreateBudget(ctx context.Context, req *connect.Request[api.CreateBudgetRequest]) (*connect.Response[api.CreateBudgetResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetBudget() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("budget is required"))
	}
	b, err := s.store.CreateBudgetAs(ctx, caller, budgetInput(req.Msg.Budget))
	if err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.CreateBudgetResponse{Budget: budgetMessage(b)}), nil
}

func (s *gatewayAdminService) UpdateBudget(ctx context.Context, req *connect.Request[api.UpdateBudgetRequest]) (*connect.Response[api.UpdateBudgetResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if req.Msg.GetBudget() == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("budget is required"))
	}
	b, err := s.store.UpdateBudgetAs(ctx, caller, req.Msg.BudgetId, budgetInput(req.Msg.Budget), req.Msg.ExpectedVersion)
	if err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.UpdateBudgetResponse{Budget: budgetMessage(b)}), nil
}

func (s *gatewayAdminService) ArchiveBudget(ctx context.Context, req *connect.Request[api.ArchiveBudgetRequest]) (*connect.Response[api.ArchiveBudgetResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.ArchiveBudgetAs(ctx, caller, req.Msg.BudgetId); err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.ArchiveBudgetResponse{}), nil
}

func (s *gatewayAdminService) ListBudgetAlerts(ctx context.Context, req *connect.Request[api.ListBudgetAlertsRequest]) (*connect.Response[api.ListBudgetAlertsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	alerts, err := s.store.BudgetAlertsAs(ctx, caller, req.Msg.OpenOnly)
	if err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.ListBudgetAlertsResponse{Alerts: alertMessages(alerts)}), nil
}

func (s *usageService) GetProjectUsage(ctx context.Context, req *connect.Request[api.GetProjectUsageRequest]) (*connect.Response[api.GetProjectUsageResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	u, err := s.store.ProjectUsageAs(ctx, caller, req.Msg.ProjectId)
	if err != nil {
		return nil, budgetError(err)
	}
	response := &api.GetProjectUsageResponse{Enabled: u.Enabled, FromDay: u.From.Format(day), ToDay: u.To.Format(day),
		Totals: totalsMessage(u.Totals), ByStage: slicesMessage(u.ByStage), ByModel: slicesMessage(u.ByModel),
		ByUser: slicesMessage(u.ByUser), ByUserHidden: u.ByUserHidden, TopRuns: slicesMessage(u.TopRuns), Alerts: alertMessages(u.Alerts)}
	if u.Budget != nil {
		response.Budget = budgetMessage(*u.Budget)
	}
	for _, p := range u.Series {
		response.Series = append(response.Series, &api.UsagePoint{Day: p.Day, Key: p.Key, CostUsdMicros: p.Cost, Tokens: p.Tokens})
	}
	return connect.NewResponse(response), nil
}

func (s *usageService) AcknowledgeBudgetAlert(ctx context.Context, req *connect.Request[api.AcknowledgeBudgetAlertRequest]) (*connect.Response[api.AcknowledgeBudgetAlertResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.store.AcknowledgeBudgetAlertAs(ctx, caller, req.Msg.AlertId); err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.AcknowledgeBudgetAlertResponse{}), nil
}

func (s *usageService) SnoozeBudgetAlert(ctx context.Context, req *connect.Request[api.SnoozeBudgetAlertRequest]) (*connect.Response[api.SnoozeBudgetAlertResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	until, err := s.store.SnoozeBudgetAlertAs(ctx, caller, req.Msg.AlertId, int(req.Msg.Hours))
	if err != nil {
		return nil, budgetError(err)
	}
	return connect.NewResponse(&api.SnoozeBudgetAlertResponse{SnoozedUntil: adminTime(until)}), nil
}
