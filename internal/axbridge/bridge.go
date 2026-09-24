// Package axbridge dispatches a fenced workflow attempt to the pinned AX API.
// A runner exit observation does not independently verify the task result.
package axbridge

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

var (
	ErrPending  = errors.New("AX launch outcome is not yet proven")
	ErrMismatch = errors.New("AX task or runtime differs from frozen attempt")
)

const syntheticShellCommand = "printf 'blaxsmith-ax-smoke-ok\\n' > /workspace/result.txt"

var syntheticCommand = []string{"/bin/sh", "-c", syntheticShellCommand}

// Task contains only fields used for deterministic dispatch.
type Task struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   TaskMetadata   `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
	Status     struct {
		Phase string `yaml:"phase"`
		Actor string `yaml:"actor"`
	} `yaml:"status,omitempty"`
}

type TaskMetadata struct {
	Name     string `yaml:"name"`
	Atespace string `yaml:"atespace"`
}

type Client interface {
	Get(context.Context, string, string) (Task, error)
	Apply(context.Context, Task) error
	Delete(context.Context, string, string) error
}

// Inspector reads the trusted Substrate control plane, not AX task status or
// a guest-supplied file. Missing/pending actors return an error for retry.
type Inspector interface {
	Current(context.Context, string, string) (bootstrap.Runtime, error)
	Gone(context.Context, string, string) (bool, error)
}

type Bridge struct {
	Workflow   *workflow.Store
	AX         Client
	Actor      Inspector
	Image      string // exact digest-pinned AX runner image
	Pool       string
	Signer     string                                              // enrolled bootstrap public key, base64
	Storage    string                                              // approved data-only snapshot location
	Tool       *tooladapter.Request                                // frozen, public CLI selection; nil runs the synthetic probe
	Workspace  string                                              // approved empty AX Workspace, bound at /workspace for tool tasks
	Gateway    string                                              // approved AX Gateway with exact public-IP egress rules
	LookupIPv4 func(context.Context, string) ([]netip.Addr, error) // nil uses system DNS
	// RevokeOwner must fence the bootstrap owner and any access lease before
	// deletion. A nil revoker fails closed.
	RevokeOwner func(context.Context, workflow.Attempt) error
}

// Name is stable across retries; AX may never receive a second identity for
// the same reserved attempt. The fence token is never placed in Task YAML.
func Name(a workflow.Attempt) (atespace, task string) {
	return Space(a.OrganizationID),
		"attempt-" + strings.ReplaceAll(a.ID, "-", "")
}

func Space(organizationID string) string {
	return "blaxsmith-" + strings.ReplaceAll(organizationID, "-", "")
}

func (b *Bridge) task(a workflow.Attempt) (Task, error) {
	if b.Workflow == nil || b.AX == nil || b.Actor == nil ||
		!strings.Contains(b.Image, "@sha256:") || b.Pool == "" || b.Signer == "" || b.Storage == "" {
		return Task{}, workflow.ErrInvalid
	}
	command, err := b.command(a)
	if err != nil {
		return Task{}, err
	}
	space, name := Name(a)
	argv := make([]any, len(command))
	for i, arg := range command {
		argv[i] = arg
	}
	spec := map[string]any{"image": b.Image, "command": argv, "debug": b.Tool == nil}
	if b.Tool != nil {
		if !axResourceName.MatchString(b.Workspace) || !axResourceName.MatchString(b.Gateway) {
			return Task{}, ErrInputs
		}
		// ponytail: one class matches the proof pool; add approved classes with admin selection.
		spec["resources"] = map[string]any{
			"requests": map[string]string{"cpu": "1", "memory": "1Gi"},
			"limits":   map[string]string{"cpu": "1", "memory": "1Gi"},
		}
		spec["workspaces"] = []any{map[string]any{"name": b.Workspace, "path": "/workspace"}}
		spec["gateway"] = map[string]any{"name": b.Gateway}
	}
	return Task{APIVersion: "ax.io/v1alpha1", Kind: "Task",
		Metadata: TaskMetadata{Name: name, Atespace: space},
		Spec:     spec,
	}, nil
}

func (b *Bridge) command(a workflow.Attempt) ([]string, error) {
	if b.Tool == nil {
		return syntheticCommand, nil
	}
	if b.Tool.AttemptID != a.ID || b.Tool.Runtime.Image != b.Image {
		return nil, workflow.ErrInvalid
	}
	return tooladapter.Command(*b.Tool)
}

// Launch requires a committed ReserveAttempt before the first network call.
// On any uncertain external outcome it fences the attempt for read-only
// reconciliation; it never retries UpdateTask blindly.
func (b *Bridge) Launch(ctx context.Context, a workflow.Attempt) (bootstrap.Runtime, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if b.Workflow == nil {
		return bootstrap.Runtime{}, workflow.ErrInvalid
	}
	var runtime bootstrap.Runtime
	err := b.Workflow.WithAttemptDispatchLock(ctx, a, func(ctx context.Context) error {
		var err error
		runtime, err = b.launch(ctx, a)
		return err
	})
	return runtime, err
}

func (b *Bridge) launch(ctx context.Context, a workflow.Attempt) (bootstrap.Runtime, error) {
	want, err := b.task(a)
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	run, state, sealed, err := b.Workflow.CurrentAttempt(ctx, a)
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	if run != "active" || state != "reserved" || !sealed {
		return bootstrap.Runtime{}, workflow.ErrFenced
	}
	got, err := b.AX.Get(ctx, want.Metadata.Atespace, want.Metadata.Name)
	if errors.Is(err, ErrNotFound) {
		if err = b.AX.Apply(ctx, want); err != nil {
			return bootstrap.Runtime{}, b.uncertain(a, fmt.Errorf("AX apply uncertain: %w", err))
		}
	} else if err != nil {
		return bootstrap.Runtime{}, b.uncertain(a, fmt.Errorf("AX read uncertain: %w", err))
	} else if !sameTask(got, want) {
		return bootstrap.Runtime{}, b.uncertain(a, ErrMismatch)
	}
	return b.confirm(ctx, a, want, false)
}

// ReconcileUnknown never writes an AX task. A missing task cannot prove that
// an earlier timed-out upsert will not arrive, so it remains reconciling.
func (b *Bridge) ReconcileUnknown(ctx context.Context, a workflow.Attempt) (bootstrap.Runtime, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if b.Workflow == nil {
		return bootstrap.Runtime{}, workflow.ErrInvalid
	}
	var runtime bootstrap.Runtime
	err := b.Workflow.WithAttemptDispatchLock(ctx, a, func(ctx context.Context) error {
		var err error
		runtime, err = b.reconcileUnknown(ctx, a)
		return err
	})
	return runtime, err
}

func (b *Bridge) reconcileUnknown(ctx context.Context, a workflow.Attempt) (bootstrap.Runtime, error) {
	want, err := b.task(a)
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	run, state, sealed, err := b.Workflow.CurrentAttempt(ctx, a)
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	if run != "active" || state != "reconciling" || !sealed {
		return bootstrap.Runtime{}, workflow.ErrFenced
	}
	return b.confirm(ctx, a, want, true)
}

func (b *Bridge) confirm(ctx context.Context, a workflow.Attempt, want Task, recovered bool) (bootstrap.Runtime, error) {
	for {
		got, err := b.AX.Get(ctx, want.Metadata.Atespace, want.Metadata.Name)
		if recovered {
			if errors.Is(err, ErrNotFound) {
				return bootstrap.Runtime{}, ErrPending
			}
			if err != nil {
				return bootstrap.Runtime{}, err
			}
		}
		if err == nil && !sameTask(got, want) {
			return bootstrap.Runtime{}, b.uncertain(a, ErrMismatch)
		}
		if err == nil && got.Status.Actor != "" && got.Status.Actor != want.Metadata.Name {
			return bootstrap.Runtime{}, b.uncertain(a, ErrMismatch)
		}
		if err == nil && (got.Status.Phase == "Failed" || got.Status.Phase == "Terminating") {
			return bootstrap.Runtime{}, b.uncertain(a, ErrMismatch)
		}
		if err == nil && got.Status.Phase != "Failed" && got.Status.Phase != "Terminating" {
			runtime, observeErr := b.Actor.Current(ctx, want.Metadata.Atespace, want.Metadata.Name)
			if errors.Is(observeErr, ErrMismatch) {
				return bootstrap.Runtime{}, b.uncertain(a, ErrMismatch)
			}
			if recovered {
				if errors.Is(observeErr, ErrPending) || errors.Is(observeErr, ErrNotFound) {
					return bootstrap.Runtime{}, ErrPending
				}
				if observeErr != nil {
					return bootstrap.Runtime{}, observeErr
				}
			}
			if observeErr == nil {
				if !b.validRuntime(runtime, want) {
					return bootstrap.Runtime{}, b.uncertain(a, ErrMismatch)
				}
				run, state, sealed, fenceErr := b.Workflow.CurrentAttempt(ctx, a)
				if fenceErr != nil || !sealed || run != "active" || (recovered && state != "reconciling") || (!recovered && state != "reserved") {
					return bootstrap.Runtime{}, workflow.ErrFenced
				}
				command, err := b.command(a)
				if err != nil {
					return bootstrap.Runtime{}, err
				}
				binding := workflow.RuntimeBinding{AXAtespace: want.Metadata.Atespace,
					AXTask: want.Metadata.Name, ActorUID: runtime.Actor.UID,
					TemplateUID: runtime.TemplateUID, Image: runtime.Image,
					WorkerPool: runtime.WorkerPool, CommandSHA256: runnerexit.CommandSHA256(command)}
				if err := b.Workflow.BindRuntime(ctx, a, binding); err != nil {
					if errors.Is(err, workflow.ErrFenced) || recovered {
						return bootstrap.Runtime{}, err
					}
					return bootstrap.Runtime{}, b.uncertain(a, err)
				}
				if recovered {
					err = b.Workflow.ConfirmRecovered(ctx, a)
				} else {
					err = b.Workflow.ConfirmStarted(ctx, a)
				}
				return runtime, err
			}
		}
		if ctx.Err() != nil {
			return bootstrap.Runtime{}, b.uncertain(a, ErrPending)
		}
		select {
		case <-ctx.Done():
			return bootstrap.Runtime{}, b.uncertain(a, ErrPending)
		case <-time.After(time.Second):
		}
	}
}

func (b *Bridge) validRuntime(r bootstrap.Runtime, want Task) bool {
	return r.Actor.Atespace == want.Metadata.Atespace && r.Actor.Name == want.Metadata.Name && r.Actor.UID != "" &&
		r.TemplateUID != "" && r.Image == b.Image && r.WorkerPool == b.Pool && r.WorkerPod != "" && r.WorkerPodUID != "" &&
		r.SandboxClass == "SANDBOX_CLASS_GVISOR" && r.BootstrapPublicKey == b.Signer &&
		r.DataOnlySnapshots() && r.SnapshotStorage == b.Storage
}

func sameTask(got, want Task) bool {
	return got.APIVersion == want.APIVersion && got.Kind == want.Kind && got.Metadata.Name == want.Metadata.Name &&
		got.Metadata.Atespace == want.Metadata.Atespace && reflect.DeepEqual(got.Spec, want.Spec)
}

func (b *Bridge) uncertain(a workflow.Attempt, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return errors.Join(cause, b.Workflow.MarkUnknown(ctx, a))
}

// StopKnown is safe only for an acknowledged attempt. A failed command can use
// this proof before retry, and cancellation uses the same path. A timed-out
// AX upsert can arrive after a delete, so reconciling attempts remain fenced
// until AX gains a server-side tombstone/compare-and-delete contract.
func (b *Bridge) StopKnown(ctx context.Context, a workflow.Attempt) error {
	if b.Workflow == nil {
		return workflow.ErrInvalid
	}
	return b.Workflow.WithAttemptDispatchLock(ctx, a, func(ctx context.Context) error {
		return b.stopKnown(ctx, a)
	})
}

func (b *Bridge) stopKnown(ctx context.Context, a workflow.Attempt) error {
	want, err := b.task(a)
	if err != nil {
		return err
	}
	run, state, _, err := b.Workflow.CurrentAttempt(ctx, a)
	if err != nil {
		return err
	}
	if (run != "active" && run != "cancel_requested") || state != "running" {
		return workflow.ErrFenced
	}
	if b.RevokeOwner == nil {
		return workflow.ErrFenced
	}
	if err := b.RevokeOwner(ctx, a); err != nil {
		return err
	}
	if err := b.AX.Delete(ctx, want.Metadata.Atespace, want.Metadata.Name); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = b.AX.Get(ctx, want.Metadata.Atespace, want.Metadata.Name)
	if !errors.Is(err, ErrNotFound) {
		if err != nil {
			return err
		}
		return ErrPending
	}
	decision, err := b.Actor.Gone(ctx, want.Metadata.Atespace, want.Metadata.Name)
	if err != nil {
		return err
	}
	if !decision {
		return ErrPending
	}
	return b.Workflow.ConfirmStopped(ctx, a)
}

// ErrNotFound is emitted only by the AX client after an authoritative GetTask
// NotFound response, not after a transport timeout.
var ErrNotFound = errors.New("AX task not found")
