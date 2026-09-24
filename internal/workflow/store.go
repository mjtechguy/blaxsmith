// Package workflow persists the first run/attempt state contract. Callers must
// authorize the organization and project before invoking this store.
package workflow

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrInvalid  = errors.New("invalid workflow input")
	ErrNotFound = errors.New("workflow resource not found")
	ErrConflict = errors.New("workflow state conflict")
	ErrFenced   = errors.New("stale attempt owner")
	ErrRecipe   = errors.New("committed recipe inputs failed validation")
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	hashPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	slugPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)
	keyPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
)

type Store struct {
	pool   *pgxpool.Pool
	authz  ConnectionAuthorizer
	access ResourceAccess   // nil: organization recipes are usable by every project.
	models ConnectionModels // nil: models already granted through the connection.
}

func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, ErrInvalid
	}
	return &Store{pool: pool, authz: grantAuthorizer{}}, nil
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
	CreatedAt          time.Time
}

type Project struct {
	ID             string
	OrganizationID string
	Slug           string
	Name           string
	CreatedAt      time.Time
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
	ID          int64
	RunID       string
	TaskID      *string
	AttemptID   *string
	Kind        string
	OccurredAt  time.Time
	PayloadJSON string // JSON object for attempt.progress; empty otherwise
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

func (s *Store) GetProject(ctx context.Context, orgID, projectID string) (Project, error) {
	if !ids(orgID, projectID) {
		return Project{}, ErrInvalid
	}
	var p Project
	err := s.pool.QueryRow(ctx, `SELECT id,organization_id,slug,name,created_at FROM workflow_projects
		WHERE organization_id=$1 AND id=$2`, orgID, projectID).
		Scan(&p.ID, &p.OrganizationID, &p.Slug, &p.Name, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// ListPage keeps search and sorting on the server; UUID breaks equal sort keys.
type ListPage struct {
	Search, Sort, Direction, AfterKey, AfterID string
	AfterTime                                  time.Time
	Limit                                      int
}

func (s *Store) ListProjects(ctx context.Context, orgID string, page ListPage) ([]Project, error) {
	if !uuidPattern.MatchString(orgID) || !validPage(page) {
		return nil, ErrInvalid
	}
	order := "created_at"
	key := any(page.AfterTime)
	if page.Sort == "name" {
		order, key = `name COLLATE "C"`, page.AfterKey
	} else if page.Sort != "created_at" {
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,organization_id,slug,name,created_at FROM workflow_projects
		WHERE organization_id=$1 AND ($2='' OR position(lower($2) in lower(name || ' ' || slug))>0)
		AND ($3='' OR (%s,id)%s($4,NULLIF($3,'')::uuid))
		ORDER BY %s %s,id %s LIMIT $5`, order, pageOperator(page.Direction), order, page.Direction, page.Direction),
		orgID, page.Search, page.AfterID, key, page.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.OrganizationID, &p.Slug, &p.Name, &p.CreatedAt); err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func (s *Store) ListRuns(ctx context.Context, orgID, projectID string, page ListPage) ([]Run, error) {
	if !ids(orgID, projectID) || !validPage(page) {
		return nil, ErrInvalid
	}
	order := "created_at"
	key := any(page.AfterTime)
	switch page.Sort {
	case "created_at":
	case "launch_key":
		order, key = `launch_key COLLATE "C"`, page.AfterKey
	case "state":
		order, key = "state", page.AfterKey
	default:
		return nil, ErrInvalid
	}
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`SELECT id,organization_id,project_id,launch_key,source_commit,
		bundle_sha256,verification_sha256,state,created_at FROM workflow_runs
		WHERE organization_id=$1 AND project_id=$2
		AND ($3='' OR position(lower($3) in lower(launch_key || ' ' || source_commit))>0)
		AND ($4='' OR (%s,id)%s($5,NULLIF($4,'')::uuid))
		ORDER BY %s %s,id %s LIMIT $6`, order, pageOperator(page.Direction), order, page.Direction, page.Direction),
		orgID, projectID, page.Search, page.AfterID, key, page.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []Run{}
	for rows.Next() {
		var r Run
		if err := rows.Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.LaunchKey, &r.SourceCommit,
			&r.BundleSHA256, &r.VerificationSHA256, &r.State, &r.CreatedAt); err != nil {
			return nil, err
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

func validPage(page ListPage) bool {
	return (page.Direction == "asc" || page.Direction == "desc") &&
		(page.AfterID == "" || uuidPattern.MatchString(page.AfterID)) && page.Limit >= 1 && page.Limit <= 101
}

func pageOperator(direction string) string {
	if direction == "asc" {
		return ">"
	}
	return "<"
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
		bundle_sha256, verification_sha256, state, created_at FROM workflow_runs
		WHERE organization_id=$1 AND project_id=$2 AND launch_key=$3`, orgID, projectID, key).
		Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.LaunchKey, &r.SourceCommit,
			&r.BundleSHA256, &r.VerificationSHA256, &r.State, &r.CreatedAt)
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
		bundle_sha256, verification_sha256, state, created_at FROM workflow_runs
		WHERE organization_id=$1 AND id=$2`, orgID, runID).
		Scan(&r.ID, &r.OrganizationID, &r.ProjectID, &r.LaunchKey, &r.SourceCommit,
			&r.BundleSHA256, &r.VerificationSHA256, &r.State, &r.CreatedAt)
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
	var graphSealed bool
	if err := tx.QueryRow(ctx, `SELECT state,graph_sealed FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&state, &graphSealed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	if state != "queued" || graphSealed {
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
	return s.reserveAttempt(ctx, orgID, runID, taskID, nil)
}

// ReserveAttemptWithBinding creates an attempt and its required access binding
// in one transaction. A denied or stale binding rolls back the reservation.
func (s *Store) ReserveAttemptWithBinding(ctx context.Context, orgID, runID, taskID string,
	bind func(context.Context, pgx.Tx, Attempt) error) (Attempt, error) {
	if bind == nil {
		return Attempt{}, ErrInvalid
	}
	return s.reserveAttempt(ctx, orgID, runID, taskID, bind)
}

func (s *Store) reserveAttempt(ctx context.Context, orgID, runID, taskID string,
	bind func(context.Context, pgx.Tx, Attempt) error) (Attempt, error) {
	if !ids(orgID, runID, taskID) {
		return Attempt{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Attempt{}, err
	}
	defer tx.Rollback(ctx)
	var runState string
	var graphSealed bool
	if err := tx.QueryRow(ctx, `SELECT state,graph_sealed FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&runState, &graphSealed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Attempt{}, ErrNotFound
		}
		return Attempt{}, err
	}
	if (runState != "queued" && runState != "active") || !graphSealed {
		return Attempt{}, ErrConflict
	}
	var taskState string
	var generation int64
	var max int
	err = tx.QueryRow(ctx, `SELECT state,generation,LEAST(20,max_attempts+extra_attempts) FROM workflow_tasks
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
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM workflow_task_dependencies d
		JOIN workflow_tasks parent ON parent.organization_id=d.organization_id
			AND parent.run_id=d.run_id AND parent.id=d.depends_on_task_id
		WHERE d.organization_id=$1 AND d.run_id=$2 AND d.task_id=$3
			AND parent.state<>'succeeded')`, orgID, runID, taskID).Scan(&blocked); err != nil {
		return Attempt{}, err
	}
	if blocked {
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
	if bind != nil {
		if err := bind(ctx, tx, a); err != nil {
			return Attempt{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Attempt{}, err
	}
	return a, nil
}

// ConfirmStarting records a confirmed actor whose Workspace is still initializing.
func (s *Store) ConfirmStarting(ctx context.Context, a Attempt) error {
	return s.transition(ctx, a, "reserved", "starting", "attempt.starting")
}

// ConfirmStarted publishes a usable attempt only after AX reports WorkspaceReady.
func (s *Store) ConfirmStarted(ctx context.Context, a Attempt) error {
	return s.transition(ctx, a, "starting", "running", "attempt.started")
}

// MarkUnknown blocks replacement and publication until the external workload
// is reconciled. It does not infer termination from a timeout.
func (s *Store) MarkUnknown(ctx context.Context, a Attempt) error {
	return s.transition(ctx, a, "reserved,starting,running", "reconciling", "attempt.unknown")
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
	if (to == "starting" || to == "running") && runState != "active" {
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
// runtime-bound success also needs a clean command exit; the caller must still
// independently verify the result. Runtime-bound failures require an actor
// stop proof before retry, so this method never releases a live AX actor.
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
	err = tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,t.generation,LEAST(20,t.max_attempts+t.extra_attempts),
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
	var runtimeBound bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_attempt_runtime
		WHERE organization_id=$1 AND attempt_id=$2)`, a.OrganizationID, a.ID).Scan(&runtimeBound); err != nil {
		return err
	}
	if runtimeBound {
		// The actor remains live after its command exits. A failed result must
		// use the connector's revoke/delete/actor-gone proof before retry.
		if !succeeded {
			return ErrConflict
		}
		var exitCode int
		var interrupted bool
		err := tx.QueryRow(ctx, `SELECT exit_code,(report_json->>'interrupted')::boolean
			FROM workflow_command_exits WHERE organization_id=$1 AND attempt_id=$2`, a.OrganizationID, a.ID).
			Scan(&exitCode, &interrupted)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (exitCode != 0 || interrupted)) {
			return ErrConflict
		}
		if err != nil {
			return err
		}
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
	err = tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,t.generation,LEAST(20,t.max_attempts+t.extra_attempts),
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
	if token == a.FenceToken && generation == a.OwnerGeneration && activeID == nil &&
		(attemptState == "succeeded" || (attemptState == "stopped" &&
			(taskState == "pending" || taskState == "blocked" || taskState == "cancelled"))) {
		return tx.Commit(ctx)
	}
	if activeID == nil || *activeID != a.ID || token != a.FenceToken || generation != a.OwnerGeneration {
		return ErrFenced
	}
	if taskState != attemptState || (taskState != "reserved" && taskState != "starting" && taskState != "running" && taskState != "reconciling") {
		return ErrConflict
	}
	if runState == "active" && taskState == "running" {
		// A recorded clean result is accepted only now that the actor is gone.
		if accepted, err := s.acceptStopped(ctx, tx, a); err != nil || accepted {
			if err != nil {
				return err
			}
			return tx.Commit(ctx)
		}
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
	if _, err := requestCancel(ctx, tx, orgID, runID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// requestCancel reports whether this call moved the run to cancel_requested.
func requestCancel(ctx context.Context, tx pgx.Tx, orgID, runID string) (bool, error) {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, runID).Scan(&state); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, ErrNotFound
		}
		return false, err
	}
	if state == "cancel_requested" {
		return false, nil
	}
	if state != "queued" && state != "active" {
		return false, ErrConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET state='cancel_requested' WHERE organization_id=$1 AND id=$2`, orgID, runID); err != nil {
		return false, err
	}
	// Pending and escalated stages have no live owner, so they cancel now.
	if _, err := tx.Exec(ctx, `UPDATE workflow_tasks SET state='cancelled' WHERE organization_id=$1
		AND run_id=$2 AND state IN ('pending','escalated')`, orgID, runID); err != nil {
		return false, err
	}
	if err := event(ctx, tx, orgID, runID, "", "", "run.cancel_requested"); err != nil {
		return false, err
	}
	return true, nil
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
	rows, err := s.pool.Query(ctx, `SELECT id,run_id,task_id::text,attempt_id::text,kind,occurred_at,COALESCE(payload::text,'')
		FROM workflow_events WHERE organization_id=$1 AND run_id=$2 AND id>$3
		ORDER BY id LIMIT $4`, orgID, runID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.RunID, &e.TaskID, &e.AttemptID, &e.Kind, &e.OccurredAt, &e.PayloadJSON); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// EventHead verifies run visibility and bounds a reconnect cursor.
func (s *Store) EventHead(ctx context.Context, orgID, runID string) (int64, error) {
	if !ids(orgID, runID) {
		return 0, ErrInvalid
	}
	var head int64
	err := s.pool.QueryRow(ctx, `SELECT event_seq FROM workflow_runs WHERE organization_id=$1 AND id=$2`, orgID, runID).Scan(&head)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	return head, err
}

func event(ctx context.Context, tx pgx.Tx, orgID, runID, taskID, attemptID, kind string) error {
	return AppendEvent(ctx, tx, orgID, runID, taskID, attemptID, kind, nil)
}

// AppendEvent writes one workflow_events row with an optional JSON object
// payload (attempt.progress carries one; other kinds pass nil).
func AppendEvent(ctx context.Context, tx pgx.Tx, orgID, runID, taskID, attemptID, kind string, payload []byte) error {
	// A per-run counter is advanced under the run row lock in this transaction.
	// Sequence IDs therefore commit in cursor order, unlike global sequences.
	var id int64
	if err := tx.QueryRow(ctx, `UPDATE workflow_runs SET event_seq=event_seq+1
		WHERE organization_id=$1 AND id=$2 RETURNING event_seq`, orgID, runID).Scan(&id); err != nil {
		return err
	}
	var task, attempt, body any
	if taskID != "" {
		task = taskID
	}
	if attemptID != "" {
		attempt = attemptID
	}
	if payload != nil {
		body = string(payload)
	}
	_, err := tx.Exec(ctx, `INSERT INTO workflow_events (id,organization_id,run_id,task_id,attempt_id,kind,payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)`, id, orgID, runID, task, attempt, kind, body)
	if err != nil {
		return err
	}
	// PostgreSQL delivers this only after commit. Durable replay remains the source of truth.
	_, err = tx.Exec(ctx, `SELECT pg_notify('blaxsmith_workflow_events',$1)`, orgID+":"+runID)
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
