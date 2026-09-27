package workflow

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrGoalAllowance = errors.New("goal execution allowance exhausted or admission deadline reached")

type GoalAllowance struct {
	StallResetAt                                *time.Time
	MaxRepeatedCheckFailures, NoProgressSeconds int
	RepeatedCheckFailures                       int64
	LastProgressAt                              *time.Time
	StallReason                                 string
	Version                                     int64
	MaxRuns, MaxAttempts                        int
	AdmitUntil                                  *time.Time
	PrincipalID                                 string
	UpdatedAt                                   *time.Time
	Runs, Attempts                              int64
	AdmissionClosed                             bool
}

func (s *Store) GetGoalAllowance(ctx context.Context, org, goal string) (GoalAllowance, error) {
	var out GoalAllowance
	if !ids(org, goal) {
		return out, ErrInvalid
	}
	ctx = tenant.Org(ctx, org)
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(a.version,0),COALESCE(a.max_runs,0),COALESCE(a.max_attempts,0),a.admit_until,COALESCE(a.principal_id::text,''),a.updated_at,COALESCE(a.max_repeated_check_failures,0),COALESCE(a.no_progress_seconds,0),a.stall_reset_at,
 (SELECT count(*) FROM workflow_goal_runs r WHERE r.organization_id=g.organization_id AND r.goal_id=g.id),
 (SELECT count(*) FROM workflow_attempts x JOIN workflow_goal_runs r ON r.organization_id=x.organization_id AND r.run_id=x.run_id WHERE r.organization_id=g.organization_id AND r.goal_id=g.id),
 COALESCE(a.admit_until<=clock_timestamp(),false)
 FROM workflow_goals g LEFT JOIN workflow_goal_allowances a ON a.organization_id=g.organization_id AND a.goal_id=g.id WHERE g.organization_id=$1 AND g.id=$2`, org, goal).Scan(&out.Version, &out.MaxRuns, &out.MaxAttempts, &out.AdmitUntil, &out.PrincipalID, &out.UpdatedAt, &out.MaxRepeatedCheckFailures, &out.NoProgressSeconds, &out.StallResetAt, &out.Runs, &out.Attempts, &out.AdmissionClosed)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNotFound
	}
	if err != nil {
		return out, err
	}
	if out.MaxRepeatedCheckFailures > 0 || out.NoProgressSeconds > 0 {
		out.RepeatedCheckFailures, out.LastProgressAt, out.StallReason, err = goalStall(ctx, s.pool, org, goal, out.MaxRepeatedCheckFailures, out.NoProgressSeconds)
		out.AdmissionClosed = out.AdmissionClosed || out.StallReason != ""
	}
	return out, err
}

// The goal row serializes run admission, attempt admission, and allowance edits.
// Counts include failed/cancelled work and unresolved dispatch intent; a rolled
// back admission consumes nothing. These are not provider token or cash caps.
func checkGoalAllowance(ctx context.Context, tx pgx.Tx, org, goal string, attempt bool) error {
	var state string
	if err := tx.QueryRow(ctx, `SELECT control_state FROM workflow_goals WHERE organization_id=$1 AND id=$2 FOR UPDATE`, org, goal).Scan(&state); err != nil {
		return err
	}
	if state != "active" {
		return ErrGoalStopped
	}
	var maxRuns, maxAttempts, repeatedFailures, noProgressSeconds int
	var expired bool
	err := tx.QueryRow(ctx, `SELECT max_runs,max_attempts,COALESCE(admit_until<=clock_timestamp(),false),max_repeated_check_failures,no_progress_seconds FROM workflow_goal_allowances WHERE organization_id=$1 AND goal_id=$2`, org, goal).Scan(&maxRuns, &maxAttempts, &expired, &repeatedFailures, &noProgressSeconds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if expired {
		return ErrGoalAllowance
	}
	if repeatedFailures > 0 || noProgressSeconds > 0 {
		_, _, reason, err := goalStall(ctx, tx, org, goal, repeatedFailures, noProgressSeconds)
		if err != nil {
			return err
		}
		if reason != "" {
			return fmt.Errorf("%w: %s", ErrGoalAllowance, reason)
		}
	}
	if !attempt && maxRuns > 0 {
		var used int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_goal_runs WHERE organization_id=$1 AND goal_id=$2`, org, goal).Scan(&used); err != nil {
			return err
		}
		if used >= int64(maxRuns) {
			return ErrGoalAllowance
		}
	}
	if maxAttempts > 0 {
		var used int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts a JOIN workflow_goal_runs g ON g.organization_id=a.organization_id AND g.run_id=a.run_id WHERE g.organization_id=$1 AND g.goal_id=$2`, org, goal).Scan(&used); err != nil {
			return err
		}
		if used >= int64(maxAttempts) {
			return ErrGoalAllowance
		}
	}
	return nil
}

// Reads only platform check observations and accepted packages. Worker text,
// token usage, leases, repeat passes on the same candidate and new run creation
// are not progress. Exact signatures deliberately do not claim semantic sameness.
// ponytail: derive from goal history; materialize counters if large goals make this query costly.
func goalStall(ctx context.Context, q reviewQuerier, org, goal string, failureLimit, noProgressSeconds int) (int64, *time.Time, string, error) {
	var repeated int64
	var check string
	var progress *time.Time
	var expired bool
	err := q.QueryRow(ctx, `WITH stall_window AS (SELECT stall_reset_at at FROM workflow_goal_allowances WHERE organization_id=$1 AND goal_id=$2), runs AS (
 SELECT r.run_id,r.created_at FROM workflow_goal_runs r WHERE r.organization_id=$1 AND r.goal_id=$2
 ), observations AS (
 SELECT e.origin_key,e.policy_sha256,e.revision,e.recorded_at,e.id,e.metadata->>'verdict' verdict,
 concat(e.metadata->>'verdict',':',e.metadata->>'exit_code',':',e.sha256,':',e.metadata->>'summary') signature
 FROM workflow_evidence e JOIN runs r ON r.run_id=e.run_id
 WHERE e.organization_id=$1 AND e.kind='verification' AND COALESCE(e.metadata->>'mode','required') IN ('','required')
 ), ranked AS (
 SELECT *,row_number() OVER w n,first_value(signature) OVER w latest FROM observations WHERE recorded_at>=COALESCE((SELECT at FROM stall_window),'-infinity'::timestamptz)
 WINDOW w AS (PARTITION BY policy_sha256,origin_key ORDER BY recorded_at DESC,id DESC)
 ), streaks AS (
 SELECT policy_sha256,origin_key,COALESCE(min(n) FILTER(WHERE signature<>latest),max(n)+1)-1 count
 FROM ranked WHERE n<=101 GROUP BY policy_sha256,origin_key
 HAVING max(verdict) FILTER(WHERE n=1) IN ('fail','blocked')
 ), accepted AS (
 SELECT min(CASE WHEN p.acceptance_mode='policy' THEN p.presented_at ELSE d.decided_at END) at FROM workflow_review_packages p JOIN runs r ON r.run_id=p.run_id

 LEFT JOIN workflow_review_decisions d ON d.organization_id=p.organization_id AND d.package_id=p.id
 WHERE p.organization_id=$1 AND (p.acceptance_mode='policy' OR d.action='approve') GROUP BY p.integrated_commit
 ), progress AS (
 SELECT min(created_at) at FROM runs UNION ALL
 SELECT min(recorded_at) at FROM observations WHERE verdict='pass' GROUP BY policy_sha256,origin_key,revision UNION ALL
 SELECT at FROM accepted
 ), last_progress AS (SELECT CASE WHEN count(at)>0 THEN greatest(max(at),(SELECT at FROM stall_window)) END at FROM progress), worst AS (
 SELECT origin_key,count FROM streaks ORDER BY count DESC,origin_key LIMIT 1
 ) SELECT COALESCE((SELECT count FROM worst),0),COALESCE((SELECT origin_key FROM worst),''),at,
 COALESCE($3::integer>0 AND at+make_interval(secs=>$3)<=clock_timestamp(),false) FROM last_progress`, org, goal, noProgressSeconds).Scan(&repeated, &check, &progress, &expired)
	if err != nil {
		return 0, nil, "", err
	}
	if failureLimit > 0 && repeated >= int64(failureLimit) {
		return repeated, progress, fmt.Sprintf("Required check %s repeated the same check failure at least %d times (limit %d).", check, repeated, failureLimit), nil
	}
	if expired {
		return repeated, progress, fmt.Sprintf("No new accepted code or passing required-check evidence within %d seconds.", noProgressSeconds), nil
	}
	return repeated, progress, "", nil
}
