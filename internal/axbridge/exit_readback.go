package axbridge

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const exitReadbackSchema = "blaxsmith.command-exit/v1alpha1"

var ErrVerificationPending = errors.New("signed command exit has no independently verified evidence")

type exitReadback struct {
	Schema          string `json:"schema"`
	ActivationNonce string `json:"activation_nonce"`
	AXAtespace      string `json:"ax_atespace"`
	AXTask          string `json:"ax_task"`
	CommandSHA256   string `json:"command_sha256"`
	ExitCode        int    `json:"exit_code"`
	Signal          int    `json:"signal"`
	Interrupted     bool   `json:"interrupted"`
	Sequence        int64  `json:"sequence"`
	ObservedAt      int64  `json:"observed_at"`
}

// CommandExitReader reaches the pinned runner through the connector-only
// Substrate router route. The route must authenticate the bearer token and
// fence the current actor UID at both router and atunnel.
type CommandExitReader struct {
	Client    *http.Client
	RouterURL string
	Token     func(context.Context) (string, error)
}

func (r *CommandExitReader) read(ctx context.Context, actor bootstrap.Actor) (exitReadback, error) {
	if r == nil || r.Client == nil || r.Token == nil || actor.Atespace == "" || actor.Name == "" || actor.UID == "" {
		return exitReadback{}, workflow.ErrInvalid
	}
	u, err := url.Parse(r.RouterURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return exitReadback{}, workflow.ErrInvalid
	}
	token, err := r.Token(ctx)
	if err != nil {
		return exitReadback{}, err
	}
	if token == "" || len(token) > 16384 || strings.ContainsAny(token, " \t\r\n") {
		return exitReadback{}, workflow.ErrInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String()+"/blaxsmith/command-exit", nil)
	if err != nil {
		return exitReadback{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("ate-target-actor", actor.Atespace+"/"+actor.Name)
	req.Header.Set("X-Blaxsmith-Actor-UID", actor.UID)
	client := *r.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return exitReadback{}, err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusAccepted {
		return exitReadback{}, ErrPending
	}
	if response.StatusCode != http.StatusOK {
		return exitReadback{}, ErrMismatch
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return exitReadback{}, ErrMismatch
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 2049))
	if err != nil || len(body) > 2048 {
		return exitReadback{}, ErrMismatch
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var observation exitReadback
	if err := decoder.Decode(&observation); err != nil {
		return exitReadback{}, ErrMismatch
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return exitReadback{}, ErrMismatch
	}
	return observation, nil
}

// CommandExitConnector signs only an authenticated current-actor observation
// whose nonce matches the latest durable bootstrap release. A clean exit does
// not certify evidence produced by the guest.
type CommandExitConnector struct {
	Bridge     *Bridge
	Reader     *CommandExitReader
	Activation interface {
		VerifyActivation(context.Context, bootstrap.Scope, bootstrap.Runtime, string) error
	}
	ClusterID string
	Signer    ed25519.PrivateKey
	Collector workflow.CommandExitCollector
	// ObserveExit is best-effort telemetry after a signed exit, before guest disposal.
	ObserveExit func(context.Context, workflow.Attempt)
}

func (c *CommandExitConnector) Collect(ctx context.Context, a workflow.Attempt) error {
	_, err := c.collect(ctx, a)
	return err
}

// Reconcile retries failed commands only after revocation and actor-gone proof.
// A clean exit remains running until an independent verifier can certify the
// frozen checks and evidence; the exit receipt alone is insufficient.
func (c *CommandExitConnector) Reconcile(ctx context.Context, a workflow.Attempt) error {
	if c == nil || c.Bridge == nil || c.Bridge.Workflow == nil || c.Collector.Store != c.Bridge.Workflow ||
		c.Collector.WorkerPool != c.Bridge.Pool {
		return workflow.ErrInvalid
	}
	run, state, sealed, err := c.Bridge.Workflow.CurrentAttempt(ctx, a)
	if err != nil {
		return err
	}
	if state != "running" || !sealed {
		return workflow.ErrFenced
	}
	if run == "cancel_requested" {
		if c.Bridge.RevokeOwner == nil {
			return workflow.ErrInvalid
		}
		return c.Bridge.StopKnown(ctx, a)
	}
	if run != "active" {
		return workflow.ErrFenced
	}
	want, err := c.Bridge.task(a)
	if err != nil {
		return err
	}
	command, err := c.Bridge.command(a)
	if err != nil {
		return err
	}
	report, binding, err := c.Collector.Load(ctx, a)
	if errors.Is(err, workflow.ErrNotFound) {
		report, err = c.collect(ctx, a)
	} else if err == nil && (binding.Image != c.Bridge.Image || binding.WorkerPool != c.Bridge.Pool ||
		binding.AXAtespace != want.Metadata.Atespace || binding.AXTask != want.Metadata.Name ||
		binding.CommandSHA256 != runnerexit.CommandSHA256(command)) {
		return ErrMismatch
	}
	if err != nil {
		return err
	}
	if c.ObserveExit != nil {
		c.ObserveExit(ctx, a)
	}
	if report.ExitCode == 0 && report.Signal == 0 && !report.Interrupted {
		return ErrVerificationPending
	}
	if c.Bridge.RevokeOwner == nil {
		return workflow.ErrInvalid
	}
	return c.Bridge.StopKnown(ctx, a)
}

func (c *CommandExitConnector) collect(ctx context.Context, a workflow.Attempt) (runnerexit.ExitReport, error) {
	if c == nil || c.Bridge == nil || c.Reader == nil || c.Activation == nil || c.ClusterID == "" ||
		len(c.Signer) != ed25519.PrivateKeySize || c.Collector.Store != c.Bridge.Workflow ||
		!c.Signer.Public().(ed25519.PublicKey).Equal(c.Collector.PublicKey) ||
		c.Collector.WorkerPool != c.Bridge.Pool {
		return runnerexit.ExitReport{}, workflow.ErrInvalid
	}
	b := c.Bridge
	want, err := b.task(a)
	if err != nil {
		return runnerexit.ExitReport{}, err
	}
	command, err := b.command(a)
	if err != nil {
		return runnerexit.ExitReport{}, err
	}
	check := func() (bootstrap.Runtime, error) {
		run, state, sealed, err := b.Workflow.CurrentAttempt(ctx, a)
		if err != nil || run != "active" || state != "running" || !sealed {
			return bootstrap.Runtime{}, workflow.ErrFenced
		}
		task, err := b.AX.Get(ctx, want.Metadata.Atespace, want.Metadata.Name)
		if err != nil || !sameTask(task, want) || task.Status.Actor != want.Metadata.Name || task.Status.Phase != "Running" {
			return bootstrap.Runtime{}, ErrMismatch
		}
		runtime, err := b.Actor.Current(ctx, want.Metadata.Atespace, want.Metadata.Name)
		if err != nil || !b.validRuntime(runtime, want) {
			return bootstrap.Runtime{}, ErrMismatch
		}
		return runtime, nil
	}
	runtime, err := check()
	if err != nil {
		return runnerexit.ExitReport{}, err
	}
	observation, err := c.Reader.read(ctx, runtime.Actor)
	if err != nil {
		return runnerexit.ExitReport{}, err
	}
	if observation.Schema != exitReadbackSchema || observation.AXAtespace != want.Metadata.Atespace ||
		observation.AXTask != want.Metadata.Name || observation.CommandSHA256 != runnerexit.CommandSHA256(command) ||
		observation.Sequence != 1 || observation.ExitCode < -1 || observation.ExitCode > 255 ||
		observation.Signal < 0 || observation.Signal > 64 || observation.ObservedAt < 1 ||
		(observation.ExitCode == -1) != (observation.Signal != 0) {
		return runnerexit.ExitReport{}, ErrMismatch
	}
	if err := c.Activation.VerifyActivation(ctx, bootstrap.Scope{ClusterID: c.ClusterID,
		AttemptID: a.ID, OwnerGeneration: a.OwnerGeneration}, runtime, observation.ActivationNonce); err != nil {
		return runnerexit.ExitReport{}, err
	}
	current, err := check()
	if err != nil || current != runtime {
		return runnerexit.ExitReport{}, workflow.ErrFenced
	}
	// No evidence was collected by this readback; the empty digest is explicit.
	empty := sha256.Sum256(nil)
	report := runnerexit.ExitReport{Schema: runnerexit.Schema, OrganizationID: a.OrganizationID,
		RunID: a.RunID, TaskID: a.TaskID, AttemptID: a.ID, OwnerGeneration: a.OwnerGeneration,
		AXAtespace: observation.AXAtespace, AXTask: observation.AXTask,
		ActorUID: runtime.Actor.UID, TemplateUID: runtime.TemplateUID,
		ActivationNonce: observation.ActivationNonce, CommandSHA256: observation.CommandSHA256,
		ExitCode: observation.ExitCode, Signal: observation.Signal, Interrupted: observation.Interrupted,
		Sequence: observation.Sequence, EvidenceSHA256: hex.EncodeToString(empty[:]), ObservedAt: observation.ObservedAt}
	signed, err := runnerexit.Sign(report, c.Signer)
	if err != nil {
		return runnerexit.ExitReport{}, err
	}
	if err := c.Collector.Record(ctx, signed); err != nil {
		return runnerexit.ExitReport{}, err
	}
	return report, nil
}
