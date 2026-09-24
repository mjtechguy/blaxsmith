package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

// Escalation asks a human to choose a remedy when a loop or human correction
// budget is exhausted. Key is stable, so a sink can deduplicate retries.
type Escalation struct {
	OrganizationID, RunID, StageKey, Key, Title, BodyMD string
	Options                                             []EscalationOption
}

type EscalationOption struct {
	ID, Label, Description string
	Recommended            bool
}

// EscalationSink is implemented by the interaction store. Raise must be
// idempotent per (run, stage, key).
type EscalationSink interface {
	Raise(context.Context, Escalation) error
}

var escalationOptions = []EscalationOption{
	{ID: "raise_cap", Label: "Raise the cap", Description: "Allow one more correction cycle.", Recommended: true},
	{ID: "accept_with_exceptions", Label: "Accept with exceptions", Description: "Accept the current result and continue."},
	{ID: "halt", Label: "Halt", Description: "Stop this stage; the run fails."},
}

type ProgressBatch struct {
	AfterOrganizationID, AfterRunID        string
	Examined, Finalized, Corrected, Raised int
	Presented                              int
}

// Progress makes one bounded pass over runs whose graph needs engine action:
// finalize and present completed runs, turn request_changes decisions into
// corrections, and raise pending escalations. Every step is idempotent.
func (s *Store) Progress(ctx context.Context, afterOrgID, afterRunID string, limit int, sink EscalationSink) (ProgressBatch, error) {
	if afterOrgID == "" {
		afterOrgID = zeroUUID
	}
	if afterRunID == "" {
		afterRunID = zeroUUID
	}
	if !ids(afterOrgID, afterRunID) || limit < 1 || limit > 100 {
		return ProgressBatch{}, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT r.organization_id,r.id FROM workflow_runs r
		WHERE (r.organization_id,r.id)>($1::uuid,$2::uuid) AND r.graph_sealed AND (
		(r.state='active' AND NOT EXISTS (SELECT 1 FROM workflow_tasks t WHERE t.organization_id=r.organization_id
			AND t.run_id=r.id AND t.state NOT IN ('succeeded','blocked')))
		OR (r.state='succeeded' AND r.review_package_id IS NULL)
		OR (r.state='succeeded' AND EXISTS (SELECT 1 FROM workflow_review_decisions d
			WHERE d.organization_id=r.organization_id AND d.run_id=r.id AND d.package_id=r.review_package_id
			AND d.action='request_changes'
			AND NOT EXISTS (SELECT 1 FROM workflow_corrections c WHERE c.organization_id=d.organization_id AND c.decision_id=d.id)
			AND NOT EXISTS (SELECT 1 FROM workflow_escalations e WHERE e.organization_id=d.organization_id AND e.decision_id=d.id)))
		OR (EXISTS (SELECT 1 FROM workflow_escalations e WHERE e.organization_id=r.organization_id
			AND e.run_id=r.id AND e.raised_at IS NULL) AND $4))
		ORDER BY r.organization_id,r.id LIMIT $3`, afterOrgID, afterRunID, limit, sink != nil)
	if err != nil {
		return ProgressBatch{}, err
	}
	type runKey struct{ org, id string }
	runs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (runKey, error) {
		var k runKey
		return k, row.Scan(&k.org, &k.id)
	})
	if err != nil {
		return ProgressBatch{}, err
	}
	var batch ProgressBatch
	var failures []error
	for _, k := range runs {
		batch.AfterOrganizationID, batch.AfterRunID = k.org, k.id
		batch.Examined++
		if err := s.progressRun(ctx, k.org, k.id, sink, &batch); err != nil {
			failures = append(failures, fmt.Errorf("progress run %s: %w", k.id, err))
		}
	}
	return batch, errors.Join(failures...)
}

func (s *Store) progressRun(ctx context.Context, orgID, runID string, sink EscalationSink, batch *ProgressBatch) error {
	run, err := s.GetRun(ctx, orgID, runID)
	if err != nil {
		return err
	}
	state := run.State
	if state == "active" {
		if state, err = s.FinalizeRun(ctx, orgID, runID); err != nil && !errors.Is(err, ErrConflict) {
			return err
		}
		if err == nil {
			batch.Finalized++
		}
	}
	if state == "succeeded" {
		if _, err := s.GetCurrentReview(ctx, orgID, runID); errors.Is(err, ErrNotFound) {
			if err := s.presentRun(ctx, run); err != nil {
				return err
			}
			batch.Presented++
		} else if err != nil {
			return err
		}
		corrected, err := s.ApplyReviewDecision(ctx, orgID, runID)
		if err != nil {
			return err
		}
		if corrected {
			batch.Corrected++
		}
	}
	if sink == nil {
		return nil
	}
	raised, err := s.raiseEscalations(ctx, orgID, runID, sink)
	batch.Raised += raised
	return err
}

// presentRun binds review to the latest accepted revision and the accepted
// result digests. ponytail: the platform does not yet run the frozen checks
// independently; this evidence digest covers guest-reported results only.
func (s *Store) presentRun(ctx context.Context, run Run) error {
	rows, err := s.pool.Query(ctx, `SELECT t.task_key,a.id,a.result_sha256,COALESCE(res.revision,'')
		FROM workflow_tasks t JOIN LATERAL (SELECT id,result_sha256,finished_at FROM workflow_attempts
			WHERE organization_id=t.organization_id AND task_id=t.id AND state='succeeded'
			ORDER BY generation DESC LIMIT 1) a ON true
		LEFT JOIN workflow_attempt_results res ON res.organization_id=t.organization_id AND res.attempt_id=a.id
		WHERE t.organization_id=$1 AND t.run_id=$2 ORDER BY a.finished_at,t.task_key`, run.OrganizationID, run.ID)
	if err != nil {
		return err
	}
	type accepted struct{ Stage, Attempt, Result, Revision string }
	results, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (accepted, error) {
		var a accepted
		var result *string
		err := row.Scan(&a.Stage, &a.Attempt, &result, &a.Revision)
		if result != nil {
			a.Result = *result
		}
		return a, err
	})
	if err != nil {
		return err
	}
	commit := run.SourceCommit
	for _, a := range results {
		if a.Revision != "" {
			commit = a.Revision // Latest accepted revision wins.
		}
	}
	evidence, err := json.Marshal(results)
	if err != nil {
		return err
	}
	_, err = s.PresentForReview(ctx, run.OrganizationID, run.ID, commit, sha(evidence), run.VerificationSHA256)
	return err
}

// ApplyReviewDecision turns the current package's request_changes decision
// into corrections on each implement stage, reopening the run. Past the
// recipe's max_correction_cycles it raises an escalation instead.
func (s *Store) ApplyReviewDecision(ctx context.Context, orgID, runID string) (bool, error) {
	if !ids(orgID, runID) {
		return false, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	var state string
	var decisionID, action, feedback *string
	err = tx.QueryRow(ctx, `SELECT r.state,d.id::text,d.action,d.feedback FROM workflow_runs r
		LEFT JOIN workflow_review_decisions d ON d.organization_id=r.organization_id AND d.run_id=r.id
			AND d.package_id=r.review_package_id
		WHERE r.organization_id=$1 AND r.id=$2 FOR UPDATE OF r`, orgID, runID).Scan(&state, &decisionID, &action, &feedback)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, err
	}
	if state != "succeeded" || decisionID == nil || *action != "request_changes" {
		return false, tx.Commit(ctx)
	}
	var handled bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_corrections WHERE organization_id=$1 AND decision_id=$2)
		OR EXISTS(SELECT 1 FROM workflow_escalations WHERE organization_id=$1 AND decision_id=$2)`,
		orgID, *decisionID).Scan(&handled); err != nil {
		return false, err
	}
	if handled {
		return false, tx.Commit(ctx)
	}
	bundle, err := runBundle(ctx, tx, orgID, runID)
	if err != nil {
		return false, err
	}
	var human string
	for _, stage := range bundle.Recipe.Stages {
		if stage.Kind == "human_review" {
			human = stage.ID
		}
	}
	left, _, err := correctionsLeft(ctx, tx, orgID, runID, human, bundle.Recipe.Limits.MaxCorrectionCycles)
	if err != nil {
		return false, err
	}
	if !left {
		if err := escalate(ctx, tx, orgID, runID, human, "", *decisionID, "review-cap-"+(*decisionID)[:8], *feedback); err != nil {
			return false, err
		}
		return false, tx.Commit(ctx)
	}
	if err := reopenForHuman(ctx, tx, orgID, runID, bundle.Recipe.Stages, human, *decisionID, *feedback); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func reopenForHuman(ctx context.Context, tx pgx.Tx, orgID, runID string, stages []recipe.Stage, human, decisionID, feedback string) error {
	for _, stage := range stages {
		if stage.Kind == "implement" {
			if err := applyCorrection(ctx, tx, orgID, runID, stage.ID, human, decisionID, feedback); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state='active' WHERE organization_id=$1 AND id=$2`, orgID, runID); err != nil {
		return err
	}
	return event(ctx, tx, orgID, runID, "", "", "run.reopened")
}

func (s *Store) raiseEscalations(ctx context.Context, orgID, runID string, sink EscalationSink) (int, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,stage_key,escalation_key,findings,task_id IS NULL FROM workflow_escalations
		WHERE organization_id=$1 AND run_id=$2 AND raised_at IS NULL ORDER BY created_at,id LIMIT 10`, orgID, runID)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id, stage, key, findings string
		human                    bool
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pending, error) {
		var p pending
		return p, row.Scan(&p.id, &p.stage, &p.key, &p.findings, &p.human)
	})
	if err != nil {
		return 0, err
	}
	raised := 0
	for _, p := range found {
		title := fmt.Sprintf("Loop %s reached its correction cap", p.stage)
		if p.human {
			title = "Human review reached max_correction_cycles"
		}
		if err := sink.Raise(ctx, Escalation{OrganizationID: orgID, RunID: runID, StageKey: p.stage, Key: p.key,
			Title: title, BodyMD: p.findings, Options: escalationOptions}); err != nil {
			return raised, err
		}
		if _, err := s.pool.Exec(ctx, `UPDATE workflow_escalations SET raised_at=clock_timestamp()
			WHERE organization_id=$1 AND id=$2 AND raised_at IS NULL`, orgID, p.id); err != nil {
			return raised, err
		}
		raised++
	}
	return raised, nil
}

// ResolveEscalation applies a human's escalation answer (raise_cap,
// accept_with_exceptions, or halt). People take over from the stage terminal,
// not from an escalation. The caller must authorize run control first.
func (s *Store) ResolveEscalation(ctx context.Context, orgID, runID, key, action string, cycles int) error {
	if !ids(orgID, runID) || (action == "raise_cap" && (cycles < 1 || cycles > 10)) ||
		(action != "raise_cap" && action != "accept_with_exceptions" && action != "halt") {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`,
		orgID, runID).Scan(&runState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	var id, stageKey, findings string
	var taskID, decisionID, resolution *string
	err = tx.QueryRow(ctx, `SELECT id,stage_key,findings,task_id::text,decision_id::text,resolution FROM workflow_escalations
		WHERE organization_id=$1 AND run_id=$2 AND escalation_key=$3 FOR UPDATE`, orgID, runID, key).
		Scan(&id, &stageKey, &findings, &taskID, &decisionID, &resolution)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if resolution != nil {
		if *resolution == action {
			return tx.Commit(ctx)
		}
		return ErrConflict
	}
	granted := 0
	if action == "raise_cap" {
		granted = cycles
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_escalations SET resolution=$3,granted_cycles=$4,resolved_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, orgID, id, action, granted); err != nil {
		return err
	}
	task := ""
	if taskID != nil {
		task = *taskID
		var state string
		if err := tx.QueryRow(ctx, `SELECT state FROM workflow_tasks WHERE organization_id=$1 AND id=$2`, orgID, task).Scan(&state); err != nil {
			return err
		}
		if state != "escalated" || runState != "active" {
			return ErrConflict
		}
		switch action {
		case "raise_cap":
			bundle, err := runBundle(ctx, tx, orgID, runID)
			if err != nil {
				return err
			}
			stage, ok := findStage(bundle, stageKey)
			if !ok || stage.Loop == nil {
				return ErrConflict
			}
			err = applyCorrection(ctx, tx, orgID, runID, stage.Loop.With, stageKey, "", findings)
			if err != nil {
				return err
			}
		case "accept_with_exceptions":
			if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded' WHERE organization_id=$1 AND id=$2`, orgID, task); err != nil {
				return err
			}
			if err := readyEvents(ctx, tx, orgID, runID, task); err != nil {
				return err
			}
		case "halt":
			if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='blocked' WHERE organization_id=$1 AND id=$2`, orgID, task); err != nil {
				return err
			}
		}
	} else if action == "raise_cap" {
		// A human-review escalation: apply the decision now that budget exists.
		if runState != "succeeded" || decisionID == nil {
			return ErrConflict
		}
		bundle, err := runBundle(ctx, tx, orgID, runID)
		if err != nil {
			return err
		}
		if err := reopenForHuman(ctx, tx, orgID, runID, bundle.Recipe.Stages, stageKey, *decisionID, findings); err != nil {
			return err
		}
	}
	if err := event(ctx, tx, orgID, runID, task, "", "task.escalation_resolved"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
