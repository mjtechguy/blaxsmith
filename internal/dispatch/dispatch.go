// Package dispatch starts bounded batches of ready, authorized AX attempts.
// It never treats process exit as task success.
package dispatch

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

var ErrNotReady = errors.New("dispatcher runtime, authority, or AX connector unavailable")

type Dispatcher struct {
	Workflow *workflow.Store
	DB       *pgxpool.Pool
	Secrets  *access.SecretStore
	Bridge   *axbridge.Bridge
	// PreflightWorker must prove this approved image/pool has the Git and
	// provider egress routes required by the frozen task. AX's default
	// deny-all gateway makes an absent preflight a hard admission failure.
	PreflightWorker func(context.Context, workflow.ApprovedToolRuntime, string, string) error
	// PreflightActivation verifies connector-side bootstrap transport and
	// credential availability before an attempt is reserved.
	PreflightActivation func(context.Context) error
	// Activate opens the AX bootstrap gate and releases the bound model lease.
	// A launched but unreleased tool task is not a started dispatch outcome.
	Activate func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error
}

type Outcome struct {
	Task      workflow.ReadyTask
	AttemptID string
	BindingID string
	State     string // blocked, conflict, started, or unresolved.
	Err       error
}

type Batch struct {
	NextOrganizationID string
	Outcomes           []Outcome
}

// DispatchBatch scans at most organizationLimit tenants and taskLimit ready
// tasks in each. Pass NextOrganizationID to the next call; empty restarts a
// round. No background loop starts without explicit product wiring.
func (d *Dispatcher) DispatchBatch(ctx context.Context, afterOrganizationID string, organizationLimit, taskLimit int) (Batch, error) {
	if d == nil || d.Workflow == nil || d.DB == nil || d.Secrets == nil || d.Bridge == nil ||
		d.Bridge.AX == nil || d.Bridge.Actor == nil || d.Bridge.Signer == "" || d.Bridge.Storage == "" ||
		d.PreflightWorker == nil || d.PreflightActivation == nil || d.Activate == nil {
		return Batch{}, ErrNotReady
	}
	organizations, err := d.Workflow.ListReadyOrganizationIDs(ctx, afterOrganizationID, organizationLimit)
	if err != nil {
		return Batch{}, err
	}
	batch := Batch{Outcomes: []Outcome{}}
	for _, orgID := range organizations {
		tasks, err := d.Workflow.ListReadyTasks(ctx, orgID, taskLimit)
		if err != nil {
			return batch, err
		}
		for _, task := range tasks {
			if err := ctx.Err(); err != nil {
				return batch, err
			}
			batch.Outcomes = append(batch.Outcomes, d.dispatchOne(ctx, task))
		}
	}
	if len(organizations) == organizationLimit {
		batch.NextOrganizationID = organizations[len(organizations)-1]
	}
	return batch, nil
}

func (d *Dispatcher) dispatchOne(ctx context.Context, candidate workflow.ReadyTask) Outcome {
	outcome := Outcome{Task: candidate, State: "blocked"}
	frozen, err := d.Workflow.LoadFrozenTask(ctx, candidate.OrganizationID, candidate.RunID, candidate.TaskID)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	approved, err := d.Workflow.GetApprovedToolRuntime(ctx, candidate.OrganizationID,
		frozen.Profile.Harness, frozen.Profile.Model, frozen.Profile.Effort)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	provider, model, err := providerModel(frozen.Profile)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	selection, err := d.Workflow.GetProjectModelGrant(ctx, candidate.OrganizationID, candidate.ProjectID, provider, model)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	prompt, artifacts, err := frozenPrompt(frozen)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	timeout := min(frozen.Bundle.Recipe.Limits.TimeoutSeconds, approved.MaxTimeoutSeconds)
	request := tooladapter.Request{AttemptID: "preflight", RepositoryURL: frozen.RepositoryURL,
		SourceRef: frozen.SourceRef, SourceCommit: frozen.Bundle.Source.Commit,
		Runtime: approved.Runtime, Profile: frozen.Profile,
		Prompt: prompt, FrozenArtifacts: artifacts, TimeoutSeconds: timeout, MaxOutputBytes: approved.MaxOutputBytes}
	if _, err := tooladapter.Command(request); err != nil {
		outcome.Err = err
		return outcome
	}
	if err := d.Bridge.CheckToolInputs(ctx, candidate.OrganizationID, frozen.RepositoryURL, provider); err != nil {
		outcome.Err = err
		return outcome
	}
	if err := d.PreflightWorker(ctx, approved, frozen.RepositoryURL, provider); err != nil {
		outcome.Err = err
		return outcome
	}
	if err := d.PreflightActivation(ctx); err != nil {
		outcome.Err = err
		return outcome
	}
	grant := access.ModelGrant{OrganizationID: candidate.OrganizationID, ProjectID: candidate.ProjectID,
		GrantID: selection.GrantID, GranteeKind: "workload", GranteeID: selection.GranteeID,
		Provider: provider, Model: model}
	authority, err := d.preflightAuthority(ctx, grant)
	if err != nil {
		outcome.Err = err
		return outcome
	}
	var bindingID string
	attempt, err := d.Workflow.ReserveAttemptWithBinding(ctx, candidate.OrganizationID, candidate.RunID, candidate.TaskID,
		func(ctx context.Context, tx pgx.Tx, attempt workflow.Attempt) error {
			if err := workflow.LockDispatchSelections(ctx, tx, candidate.OrganizationID, candidate.ProjectID,
				approved.ApprovalID, selection.SelectionID); err != nil {
				return err
			}
			var err error
			bindingID, err = access.BindModelInvoke(ctx, tx, grant, authority, attempt.ID)
			return err
		})
	if err != nil {
		outcome.Err = err
		if errors.Is(err, workflow.ErrConflict) || errors.Is(err, workflow.ErrFenced) {
			outcome.State = "conflict"
		}
		return outcome
	}
	outcome.AttemptID, outcome.BindingID = attempt.ID, bindingID
	request.AttemptID = attempt.ID
	request.SourceDirectory = "source"
	bridge := *d.Bridge // the per-attempt tool selection is never shared across launches.
	bridge.Workspace = axbridge.AttemptWorkspaceName(attempt.ID)
	if bridge.Workspace == "" {
		outcome.State, outcome.Err = "unresolved", d.markActivationUnknown(ctx, attempt, axbridge.ErrInputs)
		return outcome
	}
	bridge.Workflow, bridge.Tool, bridge.Image, bridge.Pool = d.Workflow, &request, approved.Runtime.Image, approved.WorkerPool
	runtime, err := bridge.Launch(ctx, attempt)
	if err != nil {
		outcome.State, outcome.Err = "unresolved", err
		return outcome
	}
	runtimeBinding, err := d.Workflow.GetRuntimeBinding(ctx, attempt)
	if err != nil || runtimeBinding.AXAtespace != runtime.Actor.Atespace || runtimeBinding.AXTask != runtime.Actor.Name ||
		runtimeBinding.ActorUID != runtime.Actor.UID || runtimeBinding.TemplateUID != runtime.TemplateUID ||
		runtimeBinding.Image != runtime.Image || runtimeBinding.WorkerPool != runtime.WorkerPool {
		if err == nil {
			err = axbridge.ErrMismatch
		}
		outcome.State, outcome.Err = "unresolved", d.markActivationUnknown(ctx, attempt, err)
		return outcome
	}
	// The actor is still held behind AX's bootstrap gate here. Re-read its
	// Workspace/Gateway immediately before release so an edit between admission
	// and launch cannot receive model credentials under a different policy.
	if err := bridge.CheckToolInputs(ctx, candidate.OrganizationID, frozen.RepositoryURL, provider); err != nil {
		outcome.State, outcome.Err = "unresolved", d.markActivationUnknown(ctx, attempt, err)
		return outcome
	}
	invoke := access.ModelInvoke{OrganizationID: candidate.OrganizationID, ProjectID: candidate.ProjectID,
		AttemptID: attempt.ID, BindingID: bindingID, GranteeKind: "workload", GranteeID: selection.GranteeID,
		Provider: provider, Model: model, PolicyVersion: authority.PolicyVersion}
	if err := d.Activate(ctx, attempt, runtime, runtimeBinding, invoke); err != nil {
		outcome.State, outcome.Err = "unresolved", d.markActivationUnknown(ctx, attempt, err)
		return outcome
	}
	outcome.State = "started"
	return outcome
}

func (d *Dispatcher) markActivationUnknown(ctx context.Context, attempt workflow.Attempt, cause error) error {
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return errors.Join(cause, d.Workflow.MarkUnknown(markCtx, attempt))
}

func (d *Dispatcher) preflightAuthority(ctx context.Context, grant access.ModelGrant) (access.ModelApproval, error) {
	tx, err := d.DB.Begin(ctx)
	if err != nil {
		return access.ModelApproval{}, err
	}
	defer tx.Rollback(ctx)
	approval, err := access.PreflightModelInvoke(ctx, tx, grant)
	if err != nil {
		return access.ModelApproval{}, err
	}
	secret, err := d.Secrets.ReadCurrent(ctx, tx, grant.OrganizationID, approval.Decision.ConnectionID)
	if err != nil {
		return access.ModelApproval{}, err
	}
	defer secret.Clear()
	if secret.Version != approval.SecretVersion || (secret.ExpiresAt != nil && !secret.ExpiresAt.After(time.Now())) {
		return access.ModelApproval{}, access.ErrDenied
	}
	return approval, tx.Commit(ctx)
}

func providerModel(profile recipe.Profile) (string, string, error) {
	switch profile.Harness {
	case "codex":
		return "openai", profile.Model, nil
	case "claude-code":
		return "anthropic", profile.Model, nil
	case "opencode":
		provider, model, ok := strings.Cut(profile.Model, "/")
		if ok && (provider == "openai" || provider == "anthropic") && model != "" && !strings.Contains(model, "/") {
			return provider, model, nil
		}
	}
	return "", "", tooladapter.ErrBlocked
}
