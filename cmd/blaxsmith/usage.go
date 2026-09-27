package main

import (
	"bufio"
	"bytes"
	"connectrpc.com/connect"
	"context"
	"encoding/json"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"strconv"
	"time"
)

func usageMessage(u workflow.UsageSummary) *api.UsageSummary {
	return &api.UsageSummary{Attempts: u.Attempts, ReportedAttempts: u.ReportedAttempts, Reports: u.Reports, CostReports: u.CostReports, InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CostMicrosUsd: u.CostMicrosUSD}
}
func (s *workflowService) GetRunUsage(ctx context.Context, req *connect.Request[api.GetRunUsageRequest]) (*connect.Response[api.GetRunUsageResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	value, err := s.store.GetUsage(ctx, caller.OrganizationID, req.Msg.RunId, "")
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetRunUsageResponse{Usage: usageMessage(value)}), nil
}
func (s *workflowService) GetGoalUsage(ctx context.Context, req *connect.Request[api.GetGoalUsageRequest]) (*connect.Response[api.GetGoalUsageResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	value, err := s.store.GetUsage(ctx, caller.OrganizationID, "", req.Msg.GoalId)
	if err != nil {
		return nil, workflowError(err)
	}
	return connect.NewResponse(&api.GetGoalUsageResponse{Usage: usageMessage(value)}), nil
}

// Collect only usage from the bounded tail not yet seen by the watcher. This
// closes the fast-exit race without applying or acknowledging pending questions.
// Failure leaves coverage unknown and must not prevent actor termination.
func collectExitUsage(ctx context.Context, store *workflow.Store, g interact.GuestExec, interactions *interact.Store, a workflow.Attempt) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cursor, err := interactions.Cursor(ctx, a)
	if err != nil {
		return
	}
	data, code, err := guestOutput(ctx, g, a, []string{"bx", "watch", "--cursor", strconv.FormatInt(cursor, 10), "--once"}, 4<<20)
	if err != nil || code != 0 {
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		var record struct {
			Usage *evidence.Usage `json:"usage"`
		}
		if json.Unmarshal(scanner.Bytes(), &record) == nil && record.Usage != nil {
			_ = store.RecordUsage(ctx, a, *record.Usage)
		}
	}
}
