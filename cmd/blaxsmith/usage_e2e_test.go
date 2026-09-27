package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func testUsageReporting(t *testing.T, ctx context.Context, pool *pgxpool.Pool, browser *http.Client, origin string, store *workflow.Store, a workflow.Attempt) {
	t.Helper()
	web := apiv1connect.NewWorkflowServiceClient(browser, origin+"/api")
	read := func() *api.UsageSummary {
		req := connect.NewRequest(&api.GetRunUsageRequest{RunId: a.RunID})
		req.Header().Set("Origin", origin)
		got, err := web.GetRunUsage(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.Usage
	}
	if u := read(); u.Reports != 0 || u.Attempts != 3 {
		t.Fatalf("missing usage is not zero measurement: %+v", u)
	}
	ix, err := interact.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	u := tooladapter.HarnessUsage("codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":300,"cached_input_tokens":100,"output_tokens":40}}`))
	if u == nil {
		t.Fatal("missing native report")
	}
	body, _ := json.Marshal(map[string]any{"seq": 1, "usage": u})
	// The existing attempt has completed. Late telemetry must survive terminal state.
	if err = ix.Persist(ctx, a, body); err != nil {
		t.Fatal(err)
	}
	if err = ix.Persist(ctx, a, body); err != nil {
		t.Fatal(err)
	}
	// A replay under a different transport cursor still cannot double-count.
	body, _ = json.Marshal(map[string]any{"seq": 2, "usage": u})
	if err = ix.Persist(ctx, a, body); err != nil {
		t.Fatal(err)
	}
	u.InputTokens = 999
	body, _ = json.Marshal(map[string]any{"seq": 3, "usage": u})
	if err = ix.Persist(ctx, a, body); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatalf("changed report replay: %v", err)
	}
	if cursor, err := ix.Cursor(ctx, a); err != nil || cursor != 3 {
		t.Fatalf("invalid report blocked stream: %d %v", cursor, err)
	}
	u.InputTokens = 300
	forged := a
	forged.FenceToken = "00000000-0000-0000-0000-000000000000"
	if err = store.RecordUsage(ctx, forged, *u); !errors.Is(err, workflow.ErrFenced) {
		t.Fatalf("forged attempt attribution: %v", err)
	}
	u.Harness = "claude-code"
	if err = store.RecordUsage(ctx, a, *u); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatalf("wrong harness: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE workflow_usage_reports SET input_tokens=0 WHERE organization_id=$1 AND attempt_id=$2`, a.OrganizationID, a.ID); err == nil {
		t.Fatal("usage history mutated")
	}
	if got := read(); got.InputTokens != "300" || got.OutputTokens != "40" || got.Reports != 1 || got.CostReports != 0 || got.ReportedAttempts != 1 {
		t.Fatalf("usage readback: %+v", got)
	}
	// A bounded final read repairs a missed watcher delivery without treating
	// questions as acknowledged. Replays are safe on another process/store.
	collectExitUsage(ctx, store, usageGuest{line: string(body)}, ix, a)
	report, err := store.DeliveryReport(ctx, a.OrganizationID, a.RunID)
	if err != nil || !strings.Contains(report.Markdown, "300 input tokens") || !strings.Contains(report.Markdown, "Reported cost: unknown") {
		t.Fatalf("usage export: %v", err)
	}
}

type usageGuest struct{ line string }

func (g usageGuest) Start(_ context.Context, _ workflow.Attempt, argv []string, _ bool) (*interact.Process, error) {
	if len(argv) != 5 || argv[1] != "watch" || argv[4] != "--once" {
		return nil, errors.New("unexpected usage read")
	}
	return &interact.Process{Stdout: strings.NewReader(g.line + "\n"), Wait: func() (int, error) { return 0, nil }}, nil
}
