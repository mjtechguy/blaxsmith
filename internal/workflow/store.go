// Package workflow persists the first run/attempt state contract. Callers must
// authorize the organization and project before invoking this store.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid  = errors.New("invalid workflow input")
	ErrNotFound = errors.New("workflow resource not found")
	ErrConflict = errors.New("workflow state conflict")
	ErrFenced   = errors.New("stale attempt owner")
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	keyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

type Store struct{ pool *pgxpool.Pool }

func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &Store{pool: pool}, nil
}

type RunInput struct {
	OrganizationID     string
	ProjectID          string
	LaunchKey          string
	SourceCommit       string // recipe.Freeze Bundle.Source.Commit
	BundleSHA256       string // recipe.Freeze Bundle.Digest
	VerificationSHA256 string // frozen human-controlled verification policy
}

type Run struct {
	ID                 string
	OrganizationID     string
	ProjectID          string
	LaunchKey          string
	SourceCommit       string
	BundleSHA256       string
	VerificationSHA256 string
	State              string
}

type Attempt struct {
	ID              string // compatible with bootstrap.Scope.AttemptID
	OrganizationID  string
	RunID           string
	TaskID          string
	OwnerGeneration int64  // compatible with bootstrap.Scope.OwnerGeneration
	FenceToken      string // required for owner result publication
	State           string
}

type Event struct {
	ID        int64
	RunID     string
	TaskID    *string
	AttemptID *string
	Kind      string
}

// CreateProject creates a tenant-owned project; slug conflicts are explicit.
func (s *Store) CreateProject(ctx context.Context, orgID, slug, name string) (string, error) {
	if !uuidPattern.MatchString(orgID) || !slugPattern.MatchString(slug) || strings.TrimSpace(name) == "" || len(name) > 160 {
		return "", ErrInvalid
	}
	var id string
	err := s.pool.QueryRow(ctx, `INSERT INTO workflow_projects (organization_id, slug, name)
		VALUES ($1,$2,$3) RETURNING id`, orgID, slug, name).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("create project: %w", err)
	}
	return id, nil
}

// CreateRun treats the project-scoped launch key as an idempotency key. A
// replay with changed frozen inputs is a conflict, never a new run.
func (s *Store) CreateRun(ctx context.Context, in RunInput) (Run, error) {
	if !uuidPattern.MatchString(in.OrganizationID) || !uuidPattern.MatchString(in.ProjectID) ||
		len(in.LaunchKey) < 1 || len(in.LaunchKey) > 128 || !commitPattern.MatchString(in.SourceCommit) ||
		!hashPattern.MatchString(in.BundleSHA256) || !hashPattern.MatchString(in.VerificationSHA256) {
		return Run{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Run{}, err
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_runs
		(organization_id, project_id, launch_key, source_commit, bundle_sha256, verification_sha256)
		VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (organization_id, project_id, launch_key)
		DO NOTHING RETURNING id`, in.OrganizationID, in.ProjectID, in.LaunchKey,
		in.SourceCommit, in.BundleSHA256, in.VerificationSHA256).Scan(&id)
	created := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Run{}, fmt.Errorf("create run: %w", err)
	}
	run, err := getRun(ctx, tx, in.OrganizationID, in.ProjectID, in.LaunchKey)
	if err != nil {
		return Run{}, err
	}
	if run.SourceCommit != in.SourceCommit || run.BundleSHA256 != in.BundleSHA256 || run.VerificationSHA256 != in.VerificationSHA256 {
		return Run{}, ErrConflict
	}
	if created {
		if err := event(ctx, tx, in.OrganizationID, run.ID, "", "", "run.created"); err != nil {
			return Run{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Run{}, err
	}
	return run, nil
}

func getRun(ctx context.Context, tx pgx.Tx, orgID, projectID, key string) (Run, error) {
	var r Run
	err := tx.QueryRow(ctx, `SELECT id, organization_id, project_id, launch_key, source_commit,
		bundle_sha256, verification_sha256, state FROM workflow_runs
		WHERE organization_id=$1 AND project_id=$2 AND launch_key=$3`, orgID, projectID, key).
		Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.LaunchKey, &r.SourceCommit,
			&r.BundleSHA256, &r.VerificationSHA256, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// GetRun deliberately requires both organization and run identity.
func (s *Store) GetRun(ctx context.Context, orgID, runID string) (Run, error) {
	if !uuidPattern.MatchString(orgID) || !uuidPattern.MatchString(runID) {
		return Run{}, ErrInvalid
	}
	var r Run
	err := s.pool.QueryRow(ctx, `SELECT id, organization_id, project_id, launch_key, source_commit,
		bundle_sha256, verification_sha256, state FROM workflow_runs
		WHERE organization_id=$1 AND id=$2`, orgID, runID).
		Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.LaunchKey, &r.SourceCommit,
			&r.BundleSHA256, &r.VerificationSHA256, &r.State)
	if errors.Is(err, pgx.ErrNoRows) {
		return Run{}, ErrNotFound
	}
	return r, err
}

// AddTask fixes each assignment's input digest and finite retry budget before
// the first attempt is reserved. Scope amendments need a separate contract.
func (s *Store) AddTask(ctx context.Context, orgID, runID, taskKey, inputSHA256 string, maxAttempts int) (string, error) {
	if !uuidPattern.MatchString(orgID) || !uuidPattern.MatchString(runID) || !keyPattern.MatchString(taskKey) ||
		!hashPattern.MatchString(inputSHA256) || maxAttempts < 1 || maxAttempts > 20 {
		return "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if state != "queued" {
		return "", ErrConflict
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_tasks (organization_id, run_id, task_key, input_sha256, max_attempts)
		VALUES ($1,$2,$3,$4,$5) ON CONFLICT (organization_id, run_id, task_key)
		DO NOTHING RETURNING id`, orgID, runID, taskKey, inputSHA256, maxAttempts).Scan(&id)
	created := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		var oldSHA string
		var oldMax int
		err = tx.QueryRow(ctx, `SELECT id,input_sha256,max_attempts FROM workflow_tasks
			WHERE organization_id=$1 AND run_id=$2 AND task_key=$3`, orgID, runID, taskKey).
			Scan(&id, &oldSHA, &oldMax)
		if err == nil && (oldSHA != inputSHA256 || oldMax != maxAttempts) {
			return "", ErrConflict
		}
	}
	if err != nil {
		return "", err
	}
	if created {
		if err := event(ctx, tx, orgID, runID, id, "", "task.created"); err != nil {
			return "", err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// ReserveAttempt persists dispatch intent before the AX connector is called.
// An uncertain launch must be reconciled before another attempt is reserved.
func (s *Store) ReserveAttempt(ctx context.Context, orgID, runID, taskID string) (Attempt, error) {
	if !ids(orgID, runID, taskID) {
		return Attempt{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer tx.Rollback(ctx)
	var runState string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&runState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, err
	}
	if runState != "queued" && runState != "active" {
		return Attempt{}, ErrConflict
	}
	var taskState string
	var generation int64
	var max int
	err = tx.QueryRow(ctx, `SELECT state,generation,max_attempts FROM workflow_tasks
		WHERE organization_id=$1 AND run_id=$2 AND id=$3 FOR UPDATE`, orgID, runID, taskID).
		Scan(&taskState, &generation, &max)
	if errors.Is(err, pgx.ErrNoRows) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	if taskState != "pending" || generation >= int64(max) {
		return Attempt{}, ErrConflict
	}
	var a Attempt
	a.OrganizationID, a.RunID, a.TaskID = orgID, runID, taskID
	a.OwnerGeneration, a.State = generation+1, "reserved"
	err = tx.QueryRow(ctx, `INSERT INTO workflow_attempts (organization_id,run_id,task_id,generation,state)
		VALUES ($1,$2,$3,$4,'reserved') RETURNING id,fence_token`, orgID, runID, taskID, a.OwnerGeneration).
		Scan(&a.ID, &a.FenceToken)
	if err != nil {
		return Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='reserved',generation=$4,active_attempt_id=$3
		WHERE organization_id=$1 AND id=$2`, orgID, taskID, a.ID, a.OwnerGeneration); err != nil {
		return Attempt{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state='active' WHERE organization_id=$1 AND id=$2 AND state='queued'`, orgID, runID); err != nil {
		return Attempt{}, err
	}
	if err := event(ctx, tx, orgID, runID, taskID, a.ID, "attempt.reserved"); err != nil {
		return Attempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Attempt{}, err
	}
	return a, nil
}

// ConfirmStarted records a confirmed AX launch. Duplicate confirmation is safe.
func (s *Store) ConfirmStarted(ctx context.Context, a Attempt) error {
	return s.transition(ctx, a, "reserved", "running", "attempt.started")
}

// MarkUnknown blocks replacement and publication until the external workload
// is reconciled. It does not infer termination from a timeout.
func (s *Store) MarkUnknown(ctx context.Context, a Attempt) error {
	return s.transition(ctx, a, "reserved,running", "reconciling", "attempt.unknown")
}

func (s *Store) transition(ctx context.Context, a Attempt, from, to, kind string) error {
	if !validAttempt(a) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`,
		a.OrganizationID, a.RunID).Scan(&runState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		return err
	}
	if to == "running" && runState != "active" {
		return ErrFenced
	}
	var taskState, attemptState string
	var activeID, token string
	var generation int64
	err = tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,a.state,a.fence_token,a.generation
		FROM workflow_tasks t JOIN workflow_attempts a
		ON (a.organization_id=t.organization_id AND a.task_id=t.id)
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.id=$3 AND a.id=$4
		FOR UPDATE OF t,a`, a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&taskState, &activeID, &attemptState, &token, &generation)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (activeID != a.ID || token != a.FenceToken || generation != a.OwnerGeneration)) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	if taskState == to && attemptState == to {
		return tx.Commit(ctx)
	}
	if !strings.Contains(","+from+",", ","+attemptState+",") || taskState != attemptState {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET state=$3 WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.ID, to); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state=$3 WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.TaskID, to); err != nil {
		return err
	}
	if err := event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, kind); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinishAttempt accepts a result only from the current confirmed owner. A
// failed result releases the task for a finite retry, or blocks it at budget.
func (s *Store) FinishAttempt(ctx context.Context, a Attempt, succeeded bool, resultSHA256 string) error {
	if !validAttempt(a) || !hashPattern.MatchString(resultSHA256) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`,
		a.OrganizationID, a.RunID).Scan(&runState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		return err
	}
	var taskState, attemptState, token string
	var activeID *string
	var generation int64
	var max int
	var oldResult *string
	err = tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,t.generation,t.max_attempts,
		a.state,a.fence_token,a.result_sha256 FROM workflow_tasks t
		JOIN workflow_attempts a ON (a.organization_id=t.organization_id AND a.task_id=t.id)
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.id=$3 AND a.id=$4
		FOR UPDATE OF t,a`, a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&taskState, &activeID, &generation, &max, &attemptState, &token, &oldResult)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	wantFinal := "failed"
	if succeeded {
		wantFinal = "succeeded"
	}
	if token == a.FenceToken && generation == a.OwnerGeneration && activeID == nil &&
		attemptState == wantFinal && oldResult != nil && *oldResult == resultSHA256 &&
		((succeeded && taskState == "succeeded") || (!succeeded && (taskState == "pending" || taskState == "blocked"))) {
		return tx.Commit(ctx)
	}
	if runState != "active" || activeID == nil || *activeID != a.ID || token != a.FenceToken || generation != a.OwnerGeneration {
		return ErrFenced
	}
	if taskState != "running" || attemptState != "running" {
		return ErrConflict
	}
	attemptFinal, taskFinal := "failed", "pending"
	if succeeded {
		attemptFinal, taskFinal = "succeeded", "succeeded"
	} else if generation >= int64(max) {
		taskFinal = "blocked"
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET state=$3,result_sha256=$4,finished_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.ID, attemptFinal, resultSHA256); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state=$3,active_attempt_id=NULL
		WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.TaskID, taskFinal); err != nil {
		return err
	}
	if err := event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.result"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ConfirmStopped must only follow authoritative connector evidence that the
// workload cannot still publish. It is the sole path out of reconciliation.
func (s *Store) ConfirmStopped(ctx context.Context, a Attempt) error {
	if !validAttempt(a) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`,
		a.OrganizationID, a.RunID).Scan(&runState); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrFenced
		}
		return err
	}
	var taskState, attemptState, token string
	var activeID *string
	var generation int64
	var max int
	err = tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,t.generation,t.max_attempts,
		a.state,a.fence_token FROM workflow_tasks t
		JOIN workflow_attempts a ON (a.organization_id=t.organization_id AND a.task_id=t.id)
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.id=$3 AND a.id=$4 FOR UPDATE OF t,a`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&taskState, &activeID, &generation, &max, &attemptState, &token)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	}
	if err != nil {
		return err
	}
	if token == a.FenceToken && generation == a.OwnerGeneration && activeID == nil && attemptState == "stopped" &&
		(taskState == "pending" || taskState == "blocked" || taskState == "cancelled") {
		return tx.Commit(ctx)
	}
	if activeID == nil || *activeID != a.ID || token != a.FenceToken || generation != a.OwnerGeneration {
		return ErrFenced
	}
	if taskState != attemptState || (taskState != "reserved" && taskState != "running" && taskState != "reconciling") {
		return ErrConflict
	}
	taskFinal := "pending"
	if runState == "cancel_requested" {
		taskFinal = "cancelled"
	} else if generation >= int64(max) {
		taskFinal = "blocked"
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_attempts SET state='stopped',finished_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state=$3,active_attempt_id=NULL
		WHERE organization_id=$1 AND id=$2`, a.OrganizationID, a.TaskID, taskFinal); err != nil {
		return err
	}
	if err := event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.stopped"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinalizeRun closes an executed task graph once every task has a result.
// Succeeded means engineering tasks finished; human approval is separate.
// All task mutations lock the run first, so cancellation cannot race this gate.
func (s *Store) FinalizeRun(ctx context.Context, orgID, runID string) (string, error) {
	if !ids(orgID, runID) {
		return "", ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if state == "succeeded" || state == "failed" {
		return state, tx.Commit(ctx)
	}
	if state != "active" {
		return "", ErrConflict
	}
	var total, incomplete, blocked int
	if err := tx.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE state NOT IN ('succeeded','blocked')),
		count(*) FILTER (WHERE state='blocked')
		FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2`, orgID, runID).
		Scan(&total, &incomplete, &blocked); err != nil {
		return "", err
	}
	if total == 0 || incomplete != 0 {
		return "", ErrConflict
	}
	final := "succeeded"
	if blocked > 0 {
		final = "failed"
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state=$3 WHERE organization_id=$1 AND id=$2`, orgID, runID, final); err != nil {
		return "", err
	}
	if err := event(ctx, tx, orgID, runID, "", "", "run."+final); err != nil {
		return "", err
	}
	return final, tx.Commit(ctx)
}

// RequestCancel prevents new dispatch and owner result publication. Existing
// workloads still need connector stop/reconciliation before final cancellation.
func (s *Store) RequestCancel(ctx context.Context, orgID, runID string) error {
	if !ids(orgID, runID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if state == "cancel_requested" {
		return tx.Commit(ctx)
	}
	if state != "queued" && state != "active" {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state='cancel_requested' WHERE organization_id=$1 AND id=$2`, orgID, runID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='cancelled' WHERE organization_id=$1
		AND run_id=$2 AND state='pending'`, orgID, runID); err != nil {
		return err
	}
	if err := event(ctx, tx, orgID, runID, "", "", "run.cancel_requested"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// FinalizeCancel requires all active attempts to have been confirmed stopped.
func (s *Store) FinalizeCancel(ctx context.Context, orgID, runID string) error {
	if !ids(orgID, runID) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if state == "cancelled" {
		return tx.Commit(ctx)
	}
	if state != "cancel_requested" {
		return ErrConflict
	}
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_tasks
		WHERE organization_id=$1 AND run_id=$2 AND active_attempt_id IS NOT NULL)`, orgID, runID).Scan(&active); err != nil {
		return err
	}
	if active {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state='cancelled' WHERE organization_id=$1 AND id=$2`, orgID, runID); err != nil {
		return err
	}
	if err := event(ctx, tx, orgID, runID, "", "", "run.cancelled"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EventsAfter gives a reconnectable, tenant-scoped event cursor.
func (s *Store) EventsAfter(ctx context.Context, orgID, runID string, after int64, limit int) ([]Event, error) {
	if !ids(orgID, runID) || after < 0 || limit < 1 || limit > 500 {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, `SELECT id,run_id,task_id::text,attempt_id::text,kind
		FROM workflow_events WHERE organization_id=$1 AND run_id=$2 AND id>$3
		ORDER BY id LIMIT $4`, orgID, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.TaskID, &e.AttemptID, &e.Kind); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

func event(ctx context.Context, tx pgx.Tx, orgID, runID, taskID, attemptID, kind string) error {
	// A per-run counter is advanced under the run row lock in this transaction.
	// Sequence IDs therefore commit in cursor order, unlike global sequences.
	var id int64
	if err := tx.QueryRow(ctx, `UPDATE workflow_runs SET event_seq=event_seq+1
		WHERE organization_id=$1 AND id=$2 RETURNING event_seq`, orgID, runID).Scan(&id); err != nil {
		return err
	}
	var task, attempt any
	if taskID != "" {
		task = taskID
	}
	if attemptID != "" {
		attempt = attemptID
	}
	_, err := tx.Exec(ctx, `INSERT INTO workflow_events (id,organization_id,run_id,task_id,attempt_id,kind)
		VALUES ($1,$2,$3,$4,$5,$6)`, id, orgID, runID, task, attempt, kind)
	return err
}

func ids(values ...string) bool {
	for _, value := range values {
		if !uuidPattern.MatchString(value) {
			return false
		}
	}
	return true
}

func validAttempt(a Attempt) bool {
	return ids(a.OrganizationID, a.RunID, a.TaskID, a.ID, a.FenceToken) && a.OwnerGeneration >= 1
}
