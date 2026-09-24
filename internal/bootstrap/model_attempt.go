package bootstrap

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// ModelAttempt joins one running workflow owner, its approved model binding,
// and the exact AX runtime that received the task. A connector is single-use.
type ModelAttempt struct {
	Scope   Scope
	Attempt workflow.Attempt
	Runtime workflow.RuntimeBinding
	Invoke  access.ModelInvoke
	TTL     time.Duration
}

// NewModelAttemptConnector adds fail-closed model callbacks to a host-owned
// Connector. The host supplies the trusted AX runtime reader, router mTLS,
// signer, and scheduler-authenticated assignment; no secret enters Task YAML.
func NewModelAttemptConnector(base Connector, db *pgxpool.Pool, secrets *access.SecretStore, model ModelAttempt) (Connector, error) {
	if db == nil || secrets == nil || base.Ledger == nil || base.Ledger.db != db ||
		base.Reserve != nil || base.Authorize != nil || base.Delivered != nil ||
		base.GitSetup != nil || base.ModelCredential != nil ||
		!validScope(model.Scope) || model.Scope.AttemptID != model.Attempt.ID ||
		model.Attempt.OrganizationID == "" || model.Attempt.RunID == "" || model.Attempt.TaskID == "" ||
		model.Attempt.FenceToken == "" || model.Attempt.OwnerGeneration <= 0 ||
		model.Invoke.OrganizationID != model.Attempt.OrganizationID ||
		model.Invoke.AttemptID != model.Attempt.ID || model.Invoke.BindingID == "" ||
		model.Invoke.ProjectID == "" || model.Invoke.PolicyVersion <= 0 ||
		model.Runtime.AXAtespace == "" || model.Runtime.AXTask == "" ||
		model.Runtime.ActorUID == "" || model.Runtime.TemplateUID == "" ||
		model.Runtime.Image == "" || model.Runtime.WorkerPool == "" ||
		model.Runtime.CommandSHA256 == "" || model.TTL <= 0 || model.TTL > time.Hour {
		return Connector{}, ErrDenied
	}
	var mu sync.Mutex
	var leaseID string
	var expiresAt time.Time
	base.Reserve = func(ctx context.Context, tx pgx.Tx, redeemed Redeemed) error {
		mu.Lock()
		defer mu.Unlock()
		if leaseID != "" || redeemed.Scope != model.Scope ||
			redeemed.ActorAtespace != model.Runtime.AXAtespace ||
			redeemed.ActorName != model.Runtime.AXTask || redeemed.ActorUID != model.Runtime.ActorUID {
			return ErrDenied
		}
		if err := checkModelWorkflow(ctx, tx, model); err != nil {
			return err
		}
		expiry := time.Now().Add(model.TTL)
		id, err := access.ReserveModelLease(ctx, tx, access.ModelLeaseRequest{
			OrganizationID: model.Invoke.OrganizationID, BindingID: model.Invoke.BindingID,
			ChallengeID: redeemed.ID, ClusterID: redeemed.Scope.ClusterID,
			AttemptID: redeemed.Scope.AttemptID, OwnerGeneration: redeemed.Scope.OwnerGeneration,
			ActorUID: redeemed.ActorUID, Provider: model.Invoke.Provider,
			Model: model.Invoke.Model, ExpiresAt: expiry})
		if err != nil {
			return err
		}
		leaseID, expiresAt = id, expiry
		return nil
	}
	base.Authorize = func(ctx context.Context, tx pgx.Tx, runtime Runtime) error {
		if runtime.Actor != (Actor{model.Runtime.AXAtespace, model.Runtime.AXTask, model.Runtime.ActorUID}) ||
			runtime.TemplateUID != model.Runtime.TemplateUID || runtime.Image != model.Runtime.Image ||
			runtime.WorkerPool != model.Runtime.WorkerPool {
			return ErrDenied
		}
		if err := checkModelWorkflow(ctx, tx, model); err != nil {
			return err
		}
		_, err := access.AuthorizeModelInvoke(ctx, tx, model.Invoke)
		return err
	}
	base.ModelCredential = func(ctx context.Context, tx pgx.Tx, runtime Runtime) (ModelCredential, error) {
		mu.Lock()
		id, expiry := leaseID, expiresAt
		mu.Unlock()
		if id == "" || !expiry.After(time.Now()) ||
			runtime.Actor != (Actor{model.Runtime.AXAtespace, model.Runtime.AXTask, model.Runtime.ActorUID}) ||
			runtime.TemplateUID != model.Runtime.TemplateUID || runtime.Image != model.Runtime.Image ||
			runtime.WorkerPool != model.Runtime.WorkerPool {
			return ModelCredential{}, ErrDenied
		}
		if err := checkModelWorkflow(ctx, tx, model); err != nil {
			return ModelCredential{}, err
		}
		decision, err := access.AuthorizeModelInvoke(ctx, tx, model.Invoke)
		if err != nil {
			return ModelCredential{}, err
		}
		secret, err := secrets.ReadCurrent(ctx, tx, model.Invoke.OrganizationID, decision.ConnectionID)
		if err != nil {
			return ModelCredential{}, err
		}
		if err := access.MarkLeaseAttempt(ctx, tx, model.Invoke.OrganizationID, id, decision.ConnectionID, secret.Version); err != nil {
			secret.Clear()
			return ModelCredential{}, err
		}
		return ModelCredential{AttemptID: model.Attempt.ID, Provider: model.Invoke.Provider,
			ExpiresAt: expiry, APIKey: secret.Bytes}, nil
	}
	base.Delivered = func(ctx context.Context, tx pgx.Tx, redeemed Redeemed) error {
		mu.Lock()
		id := leaseID
		mu.Unlock()
		if id == "" || redeemed.Scope != model.Scope {
			return ErrDenied
		}
		return access.MarkLeaseDelivered(ctx, tx, model.Invoke.OrganizationID, id)
	}
	return base, nil
}

// checkModelWorkflow holds the live owner and immutable runtime row through
// reservation or release, fencing cancellation and actor replacement.
func checkModelWorkflow(ctx context.Context, tx pgx.Tx, model ModelAttempt) error {
	if tx == nil {
		return ErrDenied
	}
	var runState, taskState, attemptState, token string
	var sealed bool
	var activeID *string
	var generation int64
	var runtime workflow.RuntimeBinding
	err := tx.QueryRow(ctx, `SELECT r.state,r.graph_sealed,t.state,t.active_attempt_id,a.state,a.fence_token,a.generation,
		w.ax_atespace,w.ax_task,w.actor_uid,w.template_uid,w.image,w.worker_pool,w.command_sha256
		FROM workflow_runs r JOIN workflow_tasks t
		ON t.organization_id=r.organization_id AND t.run_id=r.id
		JOIN workflow_attempts a
		ON a.organization_id=t.organization_id AND a.run_id=t.run_id AND a.task_id=t.id
		JOIN workflow_attempt_runtime w
		ON w.organization_id=a.organization_id AND w.attempt_id=a.id
		WHERE r.organization_id=$1 AND r.id=$2 AND t.id=$3 AND a.id=$4
		FOR SHARE OF r,t,a,w`, model.Attempt.OrganizationID, model.Attempt.RunID,
		model.Attempt.TaskID, model.Attempt.ID).Scan(&runState, &sealed, &taskState, &activeID,
		&attemptState, &token, &generation, &runtime.AXAtespace, &runtime.AXTask,
		&runtime.ActorUID, &runtime.TemplateUID, &runtime.Image, &runtime.WorkerPool,
		&runtime.CommandSHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return err
	}
	if runState != "active" || !sealed || taskState != "running" || attemptState != "running" ||
		activeID == nil || *activeID != model.Attempt.ID || token != model.Attempt.FenceToken ||
		generation != model.Attempt.OwnerGeneration || runtime != model.Runtime {
		return ErrDenied
	}
	return nil
}
