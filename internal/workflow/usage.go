package workflow

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// RecordUsage shares the watcher transaction and its replay cursor. Attribution
// comes from the coordinator's attempt, never from the guest report. Late
// reports remain valid after failure/cancellation: spent tokens don't disappear.
func RecordUsage(ctx context.Context, tx pgx.Tx, a Attempt, u evidence.Usage) error {
	if !validAttempt(a) || u.Validate() != nil {
		return ErrInvalid
	}
	var stageKey string
	err := tx.QueryRow(ctx, `SELECT t.task_key FROM workflow_runs r
 JOIN workflow_tasks t ON t.organization_id=r.organization_id AND t.run_id=r.id
 JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.task_id=t.id
 WHERE r.organization_id=$1 AND r.id=$2 AND t.id=$3 AND a.id=$4 AND a.fence_token=$5 AND a.generation=$6
 FOR UPDATE OF r,a`, a.OrganizationID, a.RunID, a.TaskID, a.ID, a.FenceToken, a.OwnerGeneration).Scan(&stageKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	bundle, err := runBundle(ctx, tx, a.OrganizationID, a.RunID)
	if err != nil {
		return err
	}
	stage, found := findStage(bundle, stageKey)
	if !found || stage.Kind == "verify" || bundle.Recipe.Profiles[stage.Profile].Harness != u.Harness {
		return ErrInvalid
	}
	var old evidence.Usage
	old.Key = u.Key
	err = tx.QueryRow(ctx, `SELECT harness,input_tokens,output_tokens,cost_micros_usd FROM workflow_usage_reports WHERE organization_id=$1 AND attempt_id=$2 AND report_key=$3`, a.OrganizationID, a.ID, u.Key).Scan(&old.Harness, &old.InputTokens, &old.OutputTokens, &old.CostMicrosUSD)
	if err == nil {
		previous, _ := json.Marshal(old)
		next, _ := json.Marshal(u)
		if string(previous) != string(next) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var count int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM workflow_usage_reports WHERE organization_id=$1 AND attempt_id=$2`, a.OrganizationID, a.ID).Scan(&count); err != nil {
		return err
	}
	if count >= evidence.MaxUsageReports {
		return ErrInvalid
	}
	_, err = tx.Exec(ctx, `INSERT INTO workflow_usage_reports(organization_id,run_id,task_id,attempt_id,report_key,harness,input_tokens,output_tokens,cost_micros_usd) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, a.OrganizationID, a.RunID, a.TaskID, a.ID, u.Key, u.Harness, u.InputTokens, u.OutputTokens, u.CostMicrosUSD)
	if err != nil {
		return err
	}
	// Reuse progress notifications without adding usage to acceptance evidence.
	return AppendEvent(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.progress", []byte(`{"type":"usage.recorded"}`))
}

type UsageSummary struct {
	Attempts, ReportedAttempts, Reports, CostReports int64
	InputTokens, OutputTokens, CostMicrosUSD         string // Numeric sums can exceed int64; exact decimal strings.
}

func usageSummary(ctx context.Context, tx pgx.Tx, org, id string, goal bool) (UsageSummary, error) {
	var out UsageSummary
	filter := `a.run_id=$2`
	if goal {
		filter = `EXISTS(SELECT 1 FROM workflow_goal_runs g WHERE g.organization_id=a.organization_id AND g.run_id=a.run_id AND g.goal_id=$2)`
	}
	err := tx.QueryRow(ctx, `WITH selected AS (SELECT a.id FROM workflow_attempts a WHERE a.organization_id=$1 AND `+filter+`)
 SELECT (SELECT count(*) FROM selected),count(DISTINCT u.attempt_id),count(*),count(u.cost_micros_usd),
 COALESCE(sum(u.input_tokens),0)::text,COALESCE(sum(u.output_tokens),0)::text,COALESCE(sum(u.cost_micros_usd),0)::text
 FROM workflow_usage_reports u JOIN selected a ON a.id=u.attempt_id WHERE u.organization_id=$1`, org, id).Scan(&out.Attempts, &out.ReportedAttempts, &out.Reports, &out.CostReports, &out.InputTokens, &out.OutputTokens, &out.CostMicrosUSD)
	return out, err
}

// GetUsage returns a consistent subtotal and coverage, never a claim of full
// measurement. No row means unknown, even for a successful or deterministic run.
func (s *Store) GetUsage(ctx context.Context, org, run, goal string) (UsageSummary, error) {
	if !ids(org) || (run == "") == (goal == "") {
		return UsageSummary{}, ErrInvalid
	}
	id, table := run, "workflow_runs"
	if goal != "" {
		id, table = goal, "workflow_goals"
	}
	if !ids(id) {
		return UsageSummary{}, ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return UsageSummary{}, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE organization_id=$1 AND id=$2)`, org, id).Scan(&exists); err != nil {
		return UsageSummary{}, err
	}
	if !exists {
		return UsageSummary{}, ErrNotFound
	}
	out, err := usageSummary(ctx, tx, org, id, goal != "")
	if err != nil {
		return out, err
	}
	return out, tx.Commit(ctx)
}

func (s *Store) RecordUsage(ctx context.Context, a Attempt, u evidence.Usage) error {
	ctx = tenant.Org(ctx, a.OrganizationID)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = RecordUsage(ctx, tx, a, u); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
