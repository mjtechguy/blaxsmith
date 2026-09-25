package workflow

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

const (
	attemptResultSchema = "blaxsmith.attempt-result/v1alpha1"
	maxSummaryBytes     = 16 << 10
	maxHandoffBytes     = 64 << 10
)

// AttemptResult is guest-reported handoff text. It is bounded and untrusted:
// it never proves checks passed, it only informs downstream prompts and loops.
type AttemptResult struct {
	Summary  string `json:"summary"`
	Revision string `json:"revision"`
	Verdict  string `json:"verdict"` // pass, fail, or empty for non-loop stages.
}

// ParseAttemptResult strictly decodes /tmp/blaxsmith/result.json.
func ParseAttemptResult(data []byte) (AttemptResult, error) {
	if len(data) == 0 || len(data) > 128<<10 { // 16 KiB of \u-escaped summary fits.
		return AttemptResult{}, ErrInvalid
	}
	var wire struct {
		Schema string `json:"schema"`
		AttemptResult
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || wire.Schema != attemptResultSchema ||
		!utf8.ValidString(wire.Summary) || strings.ContainsRune(wire.Summary, 0) ||
		(wire.Revision != "" && !commitPattern.MatchString(wire.Revision)) ||
		(wire.Verdict != "" && wire.Verdict != "pass" && wire.Verdict != "fail") {
		return AttemptResult{}, ErrInvalid
	}
	wire.Summary = truncateUTF8(wire.Summary, maxSummaryBytes)
	return wire.AttemptResult, nil
}

func truncateUTF8(s string, max int) string {
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// RecordAttemptResult stores the current owner's result after a clean signed
// exit. It takes effect only when ConfirmStopped proves the actor is gone. The
// first recorded result wins so a partially stopped attempt stays replayable.
func (s *Store) RecordAttemptResult(ctx context.Context, a Attempt, result AttemptResult) error {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) || len(result.Summary) > maxSummaryBytes {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState, taskState, attemptState, token string
	var activeID *string
	var generation int64
	err = tx.QueryRow(ctx, `SELECT r.state,t.state,t.active_attempt_id,a.state,a.fence_token,a.generation
		FROM workflow_runs r JOIN workflow_tasks t ON t.organization_id=r.organization_id AND t.run_id=r.id
		JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.task_id=t.id
		WHERE r.organization_id=$1 AND r.id=$2 AND t.id=$3 AND a.id=$4 FOR UPDATE OF r,t,a`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&runState, &taskState, &activeID, &attemptState, &token, &generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	if runState != "active" || activeID == nil || *activeID != a.ID || token != a.FenceToken ||
		generation != a.OwnerGeneration || taskState != "running" || attemptState != "running" {
		return ErrFenced
	}
	var clean bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_command_exits WHERE organization_id=$1
		AND attempt_id=$2 AND exit_code=0 AND NOT (report_json->>'interrupted')::boolean
		AND (report_json->>'signal')::integer=0)`, a.OrganizationID, a.ID).Scan(&clean); err != nil {
		return err
	}
	if !clean {
		return ErrConflict
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var revision any
	if result.Revision != "" {
		revision = result.Revision
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_attempt_results
		(organization_id,attempt_id,run_id,task_id,summary,revision,verdict,result_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT (organization_id,attempt_id) DO NOTHING`,
		a.OrganizationID, a.ID, a.RunID, a.TaskID, result.Summary, revision, result.Verdict, sha(encoded)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// HasAttemptResult lets completion resume a stop without re-reading a guest
// that may already be gone.
func (s *Store) HasAttemptResult(ctx context.Context, a Attempt) (bool, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) {
		return false, ErrInvalid
	}
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_attempt_results
		WHERE organization_id=$1 AND attempt_id=$2)`, a.OrganizationID, a.ID).Scan(&found)
	return found, err
}

// acceptStopped runs inside ConfirmStopped once actor absence is proved. It
// reports false when no result was recorded, leaving the normal retry path.
func (s *Store) acceptStopped(ctx context.Context, tx pgx.Tx, a Attempt) (bool, error) {
	var result AttemptResult
	var resultSHA string
	err := tx.QueryRow(ctx, `SELECT summary,COALESCE(revision,''),verdict,result_sha256 FROM workflow_attempt_results
		WHERE organization_id=$1 AND attempt_id=$2`, a.OrganizationID, a.ID).
		Scan(&result.Summary, &result.Revision, &result.Verdict, &resultSHA)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var stale bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM workflow_task_dependencies d
		JOIN workflow_tasks parent ON parent.organization_id=d.organization_id AND parent.id=d.depends_on_task_id
		WHERE d.organization_id=$1 AND d.run_id=$2 AND d.task_id=$3 AND parent.state<>'succeeded')`,
		a.OrganizationID, a.RunID, a.TaskID).Scan(&stale); err != nil {
		return false, err
	}
	if stale {
		// An upstream correction reset this task's inputs while it ran.
		if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET state='stopped',finished_at=clock_timestamp()
			WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.ID); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='pending',active_attempt_id=NULL,
			extra_attempts=LEAST(19,extra_attempts+1) WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.TaskID); err != nil {
			return false, err
		}
		return true, event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.stopped")
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET state='succeeded',result_sha256=$3,finished_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.ID, resultSHA); err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded',active_attempt_id=NULL
		WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.TaskID); err != nil {
		return false, err
	}
	if err := event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.result"); err != nil {
		return false, err
	}
	bundle, err := runBundle(ctx, tx, a.OrganizationID, a.RunID)
	if errors.Is(err, ErrNotFound) { // A run without a frozen recipe has no loops.
		return true, readyEvents(ctx, tx, a.OrganizationID, a.RunID, a.TaskID)
	}
	if err != nil {
		return false, err
	}
	var key string
	if err := tx.QueryRow(ctx, `SELECT task_key FROM workflow_tasks WHERE organization_id=$1 AND id=$2`,
		a.OrganizationID, a.TaskID).Scan(&key); err != nil {
		return false, err
	}
	stage, ok := findStage(bundle, key)
	if !ok {
		return false, ErrConflict
	}
	if stage.Loop == nil || result.Verdict == "pass" {
		return true, readyEvents(ctx, tx, a.OrganizationID, a.RunID, a.TaskID)
	}
	left, used, err := correctionsLeft(ctx, tx, a.OrganizationID, a.RunID, stage.ID, stage.Loop.MaxCycles)
	if err != nil {
		return false, err
	}
	payload, err := json.Marshal(map[string]any{"type": "cycle", "cycle": used + 1, "max_cycles": stage.Loop.MaxCycles,
		"status": "fail", "text": truncateUTF8(result.Summary, 2000)})
	if err != nil {
		return false, err
	}
	if err := AppendEvent(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.progress", payload); err != nil {
		return false, err
	}
	if !left {
		return true, escalate(ctx, tx, a.OrganizationID, a.RunID, stage.ID, a.TaskID, "",
			fmt.Sprintf("loop-cap-%s-%d", stage.ID, used), result.Summary)
	}
	return true, applyCorrection(ctx, tx, a.OrganizationID, a.RunID, stage.Loop.With, stage.ID, "", result.Summary)
}

// correctionsLeft counts one correction per loop failure or human decision.
func correctionsLeft(ctx context.Context, tx pgx.Tx, orgID, runID, source string, limit int) (bool, int, error) {
	var used, granted int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(DISTINCT COALESCE(decision_id,id)) FROM workflow_corrections
			WHERE organization_id=$1 AND run_id=$2 AND source_stage=$3),
		(SELECT COALESCE(sum(granted_cycles),0) FROM workflow_escalations
			WHERE organization_id=$1 AND run_id=$2 AND stage_key=$3 AND resolution='raise_cap')`,
		orgID, runID, source).Scan(&used, &granted); err != nil {
		return false, 0, err
	}
	return used < limit+granted, used, nil
}

// applyCorrection reruns target and every finished descendant. Running
// descendants are rejected as stale when they finish.
func applyCorrection(ctx context.Context, tx pgx.Tx, orgID, runID, targetKey, source, decisionID, message string) error {
	var targetID string
	if err := tx.QueryRow(ctx, `SELECT id FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2 AND task_key=$3`,
		orgID, runID, targetKey).Scan(&targetID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	if _, err := tx.Exec(ctx, `WITH RECURSIVE down(id) AS (SELECT $3::uuid
		UNION SELECT d.task_id FROM workflow_task_dependencies d JOIN down ON d.depends_on_task_id=down.id
		WHERE d.organization_id=$1 AND d.run_id=$2)
		UPDATE workflow_tasks t SET extra_attempts=LEAST(19,t.extra_attempts+CASE WHEN t.state='pending' THEN 0 ELSE 1 END),
		state='pending' FROM down WHERE t.organization_id=$1 AND t.id=down.id
		AND t.state IN ('succeeded','pending','blocked','escalated')`, orgID, runID, targetID); err != nil {
		return err
	}
	var decision any
	if decisionID != "" {
		decision = decisionID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_corrections (organization_id,run_id,task_id,source_stage,decision_id,message)
		VALUES ($1,$2,$3,$4,$5,$6)`, orgID, runID, targetID, source, decision, truncateUTF8(message, maxSummaryBytes)); err != nil {
		return err
	}
	return event(ctx, tx, orgID, runID, targetID, "", "task.correction")
}

func escalate(ctx context.Context, tx pgx.Tx, orgID, runID, stageKey, taskID, decisionID, key, findings string) error {
	var task, decision any
	if taskID != "" {
		task = taskID
		if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='escalated',active_attempt_id=NULL
			WHERE organization_id=$1 AND id=$2`, orgID, taskID); err != nil {
			return err
		}
	}
	if decisionID != "" {
		decision = decisionID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_escalations
		(organization_id,run_id,stage_key,escalation_key,task_id,decision_id,findings) VALUES ($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT (organization_id,run_id,escalation_key) DO NOTHING`, orgID, runID, stageKey, key,
		task, decision, truncateUTF8(findings, maxSummaryBytes)); err != nil {
		return err
	}
	return event(ctx, tx, orgID, runID, taskID, "", "task.escalated")
}

func readyEvents(ctx context.Context, tx pgx.Tx, orgID, runID, parentID string) error {
	rows, err := tx.Query(ctx, `SELECT t.id FROM workflow_task_dependencies d
		JOIN workflow_tasks t ON t.organization_id=d.organization_id AND t.id=d.task_id
		WHERE d.organization_id=$1 AND d.run_id=$2 AND d.depends_on_task_id=$3 AND t.state='pending'
		AND NOT EXISTS (SELECT 1 FROM workflow_task_dependencies d2
			JOIN workflow_tasks p ON p.organization_id=d2.organization_id AND p.id=d2.depends_on_task_id
			WHERE d2.organization_id=t.organization_id AND d2.task_id=t.id AND p.state<>'succeeded')
		ORDER BY t.created_at,t.id`, orgID, runID, parentID)
	if err != nil {
		return err
	}
	ready, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ready {
		if err := event(ctx, tx, orgID, runID, id, "", "task.ready"); err != nil {
			return err
		}
	}
	return nil
}

func runBundle(ctx context.Context, q reviewQuerier, orgID, runID string) (*recipe.Bundle, error) {
	var sourceCommit, bundleSHA, verificationSHA string
	var bundleJSON, verificationJSON []byte
	err := q.QueryRow(ctx, `SELECT r.source_commit,r.bundle_sha256,r.verification_sha256,b.bundle_json,b.verification_json
		FROM workflow_runs r JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		WHERE r.organization_id=$1 AND r.id=$2`, orgID, runID).
		Scan(&sourceCommit, &bundleSHA, &verificationSHA, &bundleJSON, &verificationJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	bundle, _, err := decodeFrozenBundle(sourceCommit, bundleSHA, verificationSHA, bundleJSON, verificationJSON)
	return bundle, err
}

func findStage(bundle *recipe.Bundle, id string) (recipe.Stage, bool) {
	for _, stage := range bundle.Recipe.Stages {
		if stage.ID == id {
			return stage, true
		}
	}
	return recipe.Stage{}, false
}

// BuildHandoff renders the bounded upstream context for a task's next
// attempt: each direct upstream stage's accepted summary and revision, plus
// unconsumed correction requests. RecordHandoff freezes it with the attempt.
func (s *Store) BuildHandoff(ctx context.Context, orgID, runID, taskID string) (string, []string, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, runID, taskID) {
		return "", nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT parent.task_key,COALESCE(a.id::text,''),COALESCE(res.summary,''),COALESCE(res.revision,'')
		FROM workflow_task_dependencies d
		JOIN workflow_tasks parent ON parent.organization_id=d.organization_id AND parent.id=d.depends_on_task_id
		LEFT JOIN LATERAL (SELECT id FROM workflow_attempts WHERE organization_id=parent.organization_id
			AND task_id=parent.id AND state='succeeded' ORDER BY generation DESC LIMIT 1) a ON true
		LEFT JOIN workflow_attempt_results res ON res.organization_id=d.organization_id AND res.attempt_id=a.id
		WHERE d.organization_id=$1 AND d.run_id=$2 AND d.task_id=$3 ORDER BY parent.task_key`, orgID, runID, taskID)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var key, attemptID, summary, revision string
	_, err = pgx.ForEachRow(rows, []any{&key, &attemptID, &summary, &revision}, func() error {
		fmt.Fprintf(&b, "\n### Upstream stage %s (attempt %s)\nResulting revision: %s\nFinal summary:\n%s\n",
			key, orDefault(attemptID, "none"), orDefault(revision, "unchanged"), orDefault(summary, "(no summary recorded)"))
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	rows, err = s.pool.Query(ctx, `SELECT id,source_stage,message FROM workflow_corrections
		WHERE organization_id=$1 AND run_id=$2 AND task_id=$3 AND consumed_attempt_id IS NULL
		ORDER BY created_at,id`, orgID, runID, taskID)
	if err != nil {
		return "", nil, err
	}
	corrections := []string{}
	var id, source, message string
	_, err = pgx.ForEachRow(rows, []any{&id, &source, &message}, func() error {
		corrections = append(corrections, id)
		fmt.Fprintf(&b, "\n### Correction requested by %s\n%s\n", source, message)
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return truncateUTF8(b.String(), maxHandoffBytes), corrections, nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// RecordHandoff runs inside the reservation transaction so a correction is
// consumed by exactly one attempt.
func RecordHandoff(ctx context.Context, tx pgx.Tx, a Attempt, handoff string, corrections []string) error {
	if handoff == "" && len(corrections) == 0 {
		return nil
	}
	if len(handoff) > maxHandoffBytes {
		return ErrInvalid
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workflow_attempt_handoffs
		(organization_id,attempt_id,run_id,task_id,handoff,handoff_sha256) VALUES ($1,$2,$3,$4,$5,$6)`,
		a.OrganizationID, a.ID, a.RunID, a.TaskID, handoff, sha([]byte(handoff))); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE workflow_corrections SET consumed_attempt_id=$3
		WHERE organization_id=$1 AND task_id=$2 AND id=ANY($4::uuid[]) AND consumed_attempt_id IS NULL`,
		a.OrganizationID, a.TaskID, a.ID, corrections)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != int64(len(corrections)) {
		return ErrConflict
	}
	return nil
}

// LoadHandoff returns the exact handoff frozen into an attempt's prompt.
func (s *Store) LoadHandoff(ctx context.Context, a Attempt) (string, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) {
		return "", ErrInvalid
	}
	var handoff, digest string
	err := s.pool.QueryRow(ctx, `SELECT handoff,handoff_sha256 FROM workflow_attempt_handoffs
		WHERE organization_id=$1 AND attempt_id=$2`, a.OrganizationID, a.ID).Scan(&handoff, &digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err == nil && sha([]byte(handoff)) != digest {
		return "", ErrConflict
	}
	return handoff, err
}
