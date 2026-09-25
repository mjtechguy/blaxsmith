package workflow

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var imageDigestPattern = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

type RuntimeBinding struct {
	AXAtespace    string
	AXTask        string
	ActorUID      string
	TemplateUID   string
	Image         string
	WorkerPool    string
	CommandSHA256 string
}

// BindRuntime pins the read-back AX/Substrate identity before the attempt is
// acknowledged as running. Only the current reserved/reconciling owner may bind.
func (s *Store) BindRuntime(ctx context.Context, a Attempt, b RuntimeBinding) error {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) || b.AXAtespace == "" || b.AXTask == "" || b.ActorUID == "" ||
		b.TemplateUID == "" || b.WorkerPool == "" || !imageDigestPattern.MatchString(b.Image) ||
		!hashPattern.MatchString(b.CommandSHA256) {
		return ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var runState string
	var sealed bool
	if err := tx.QueryRow(ctx, `SELECT state,graph_sealed FROM workflow_runs
		WHERE organization_id=$1 AND id=$2 FOR UPDATE`, a.OrganizationID, a.RunID).
		Scan(&runState, &sealed); errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	} else if err != nil {
		return err
	}
	if runState != "active" || !sealed {
		return ErrFenced
	}
	var taskState, attemptState, activeID, token string
	var generation int64
	if err := tx.QueryRow(ctx, `SELECT t.state,t.active_attempt_id,a.state,a.fence_token,a.generation
		FROM workflow_tasks t JOIN workflow_attempts a
		ON a.organization_id=t.organization_id AND a.task_id=t.id
		WHERE t.organization_id=$1 AND t.run_id=$2 AND t.id=$3 AND a.id=$4
		FOR UPDATE OF t,a`, a.OrganizationID, a.RunID, a.TaskID, a.ID).
		Scan(&taskState, &activeID, &attemptState, &token, &generation); errors.Is(err, pgx.ErrNoRows) {
		return ErrFenced
	} else if err != nil {
		return err
	}
	if activeID != a.ID || token != a.FenceToken || generation != a.OwnerGeneration || taskState != attemptState ||
		(taskState != "reserved" && taskState != "starting" && taskState != "reconciling" && taskState != "running") {
		return ErrFenced
	}
	var createdID string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_attempt_runtime
		(organization_id,run_id,task_id,attempt_id,ax_atespace,ax_task,actor_uid,
		 template_uid,image,worker_pool,command_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (organization_id,attempt_id) DO NOTHING RETURNING attempt_id`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID, b.AXAtespace, b.AXTask,
		b.ActorUID, b.TemplateUID, b.Image, b.WorkerPool, b.CommandSHA256).Scan(&createdID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if errors.Is(err, pgx.ErrNoRows) {
		var old RuntimeBinding
		if err := tx.QueryRow(ctx, `SELECT ax_atespace,ax_task,actor_uid,template_uid,image,worker_pool,command_sha256
			FROM workflow_attempt_runtime WHERE organization_id=$1 AND attempt_id=$2`,
			a.OrganizationID, a.ID).Scan(&old.AXAtespace, &old.AXTask, &old.ActorUID,
			&old.TemplateUID, &old.Image, &old.WorkerPool, &old.CommandSHA256); err != nil {
			return err
		}
		if old != b {
			return ErrConflict
		}
	} else if err := event(ctx, tx, a.OrganizationID, a.RunID, a.TaskID, a.ID, "attempt.runtime_bound"); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetRuntimeBinding returns the actor identity already read back and pinned
// for this exact attempt. Callers still recheck current ownership before use.
func (s *Store) GetRuntimeBinding(ctx context.Context, a Attempt) (RuntimeBinding, error) {
	ctx = tenant.Org(ctx, a.OrganizationID)
	if !validAttempt(a) {
		return RuntimeBinding{}, ErrInvalid
	}
	var b RuntimeBinding
	err := s.pool.QueryRow(ctx, `SELECT ax_atespace,ax_task,actor_uid,template_uid,image,worker_pool,command_sha256
		FROM workflow_attempt_runtime WHERE organization_id=$1 AND run_id=$2 AND task_id=$3 AND attempt_id=$4`,
		a.OrganizationID, a.RunID, a.TaskID, a.ID).Scan(&b.AXAtespace, &b.AXTask, &b.ActorUID,
		&b.TemplateUID, &b.Image, &b.WorkerPool, &b.CommandSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeBinding{}, ErrNotFound
	}
	if err != nil {
		return RuntimeBinding{}, err
	}
	if b.AXAtespace == "" || b.AXTask == "" || b.ActorUID == "" || b.TemplateUID == "" ||
		!imageDigestPattern.MatchString(b.Image) || b.WorkerPool == "" || !hashPattern.MatchString(b.CommandSHA256) {
		return RuntimeBinding{}, ErrConflict
	}
	return b, nil
}
