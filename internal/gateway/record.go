package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Event is one completed or failed upstream request (§7). It holds counts
// and metadata only; no prompt or response content.
type Event struct {
	Grant                   Grant
	RouteID, RouteKind, API string
	RequestedModel          string
	Status                  string // ok, error, cancelled
	HTTPStatus              int
	Streamed                bool
	RequestID               string
	StartedAt               time.Time
	TTFT                    *time.Duration
	Duration                time.Duration
	Usage                   Usage
	Price                   Price
}

// Record writes the event and bumps the run's totals in one transaction.
func Record(ctx context.Context, db *pgxpool.Pool, e Event) error {
	ctx = tenant.Org(ctx, e.Grant.OrganizationID)
	if db == nil {
		return ErrDenied
	}
	g := e.Grant
	cost := Cost(e.Usage, e.Price.Rates)
	var ttft *int64
	if e.TTFT != nil {
		ms := e.TTFT.Milliseconds()
		ttft = &ms
	}
	errors := 0
	if e.Status != "ok" {
		errors = 1
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO gateway_usage_events
		(organization_id,project_id,run_id,attempt_id,task_id,stage_key,principal_id,harness,route_id,route_kind,api,
		 requested_model,served_model,status,http_status,streamed,usage_reported,request_id,started_at,ttft_ms,duration_ms,
		 input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,cost_usd_micros,price_version)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)`,
		g.OrganizationID, g.ProjectID, g.RunID, g.AttemptID, g.TaskID, g.StageKey, g.PrincipalID, g.Harness,
		e.RouteID, e.RouteKind, e.API, e.RequestedModel, e.Usage.ServedModel, e.Status, e.HTTPStatus, e.Streamed,
		e.Usage.Reported, e.RequestID, e.StartedAt, ttft, e.Duration.Milliseconds(),
		e.Usage.Input, e.Usage.Output, e.Usage.CacheRead, e.Usage.CacheWrite, e.Usage.Reasoning, cost, e.Price.Version); err != nil {
		return fmt.Errorf("record gateway usage: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gateway_run_usage AS u
		(organization_id,run_id,project_id,principal_id,requests,errors,input_tokens,output_tokens,cache_read_tokens,
		 cache_write_tokens,reasoning_tokens,cost_usd_micros,last_request_at)
		VALUES ($1,$2,$3,$4,1,$5,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT (organization_id,run_id) DO UPDATE SET requests=u.requests+1, errors=u.errors+EXCLUDED.errors,
		input_tokens=u.input_tokens+EXCLUDED.input_tokens, output_tokens=u.output_tokens+EXCLUDED.output_tokens,
		cache_read_tokens=u.cache_read_tokens+EXCLUDED.cache_read_tokens,
		cache_write_tokens=u.cache_write_tokens+EXCLUDED.cache_write_tokens,
		reasoning_tokens=u.reasoning_tokens+EXCLUDED.reasoning_tokens,
		cost_usd_micros=u.cost_usd_micros+EXCLUDED.cost_usd_micros,
		last_request_at=GREATEST(u.last_request_at,EXCLUDED.last_request_at)`,
		g.OrganizationID, g.RunID, g.ProjectID, g.PrincipalID, errors, e.Usage.Input, e.Usage.Output,
		e.Usage.CacheRead, e.Usage.CacheWrite, e.Usage.Reasoning, cost, e.StartedAt); err != nil {
		return fmt.Errorf("record run usage: %w", err)
	}
	return tx.Commit(ctx)
}

// Rollup recomputes gateway_usage_daily for every day from since (UTC)
// onward. It is idempotent: each tick recomputes today and yesterday, so a
// stream that crosses midnight is still counted on the day it started.
func Rollup(ctx context.Context, db *pgxpool.Pool, since time.Time) error {
	ctx = tenant.System(ctx)
	day := since.UTC().Truncate(24 * time.Hour)
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('blaxsmith'), hashtext('gateway_rollup'))`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM gateway_usage_daily WHERE day>=$1::date`, day); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO gateway_usage_daily
		(organization_id,day,project_id,principal_id,pool_id,route_id,route_kind,model,requests,errors,rate_limited,
		 input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,cost_usd_micros,ttft_ms_sum,ttft_count)
		SELECT organization_id,(started_at AT TIME ZONE 'UTC')::date,project_id,principal_id,pool_id,route_id,
			min(route_kind),COALESCE(NULLIF(served_model,''),requested_model),
			count(*),count(*) FILTER (WHERE status<>'ok'),count(*) FILTER (WHERE http_status=429),
			sum(input_tokens),sum(output_tokens),sum(cache_read_tokens),sum(cache_write_tokens),sum(reasoning_tokens),
			sum(cost_usd_micros),COALESCE(sum(ttft_ms),0),count(ttft_ms)
		FROM gateway_usage_events WHERE started_at>=$1
		GROUP BY 1,2,3,4,5,6,8`, day); err != nil {
		return fmt.Errorf("gateway rollup: %w", err)
	}
	return tx.Commit(ctx)
}

// EnsurePartitions creates this month's and the next two months' event
// partitions; the migration seeds them and the rollup loop keeps ahead.
func EnsurePartitions(ctx context.Context, db *pgxpool.Pool) error {
	ctx = tenant.System(ctx)
	_, err := db.Exec(ctx, `SELECT gateway_ensure_usage_partition((date_trunc('month', clock_timestamp()) + make_interval(months => m))::date)
		FROM generate_series(0, 2) AS m`)
	return err
}

// PruneEvents drops raw events older than the retention period (§7, 90 days
// by default). Rollups and run totals are kept.
func PruneEvents(ctx context.Context, db *pgxpool.Pool, retention time.Duration) error {
	ctx = tenant.System(ctx)
	if retention < 24*time.Hour {
		return ErrDenied
	}
	_, err := db.Exec(ctx, `DELETE FROM gateway_usage_events WHERE started_at<clock_timestamp()-$1::interval`, retention)
	return err
}
