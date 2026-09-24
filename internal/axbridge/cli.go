package axbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"gopkg.in/yaml.v3"
)

// CLI uses the pinned AX CLI over a local Kubernetes tunnel. AX's current
// gRPC client uses plaintext even for an https URL, so a non-loopback endpoint
// is rejected instead of implying transport security it does not provide.
type CLI struct {
	AXPath       string
	Server       string
	AtePath      string
	AteEndpoint  string
	AteTokenFile string
	AteCAFile    string
}

func (c CLI) ax(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	u, err := url.Parse(c.Server)
	if err != nil || u.Scheme != "http" || u.Port() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("AX requires a loopback tunnel URL")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() || c.AXPath == "" {
		return nil, errors.New("AX requires a loopback tunnel URL and pinned CLI")
	}
	cmd := exec.CommandContext(ctx, c.AXPath, append([]string{"--server", c.Server}, args...)...) // #nosec G204 -- pinned AX executable with separate argv; no shell
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "code = NotFound") {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("AX %s: %w", args[0], err)
	}
	return out, nil
}

func (c CLI) Get(ctx context.Context, space, name string) (Task, error) {
	out, err := c.ax(ctx, nil, "get", "task", name, "--atespace", space)
	if err != nil {
		return Task{}, err
	}
	var task Task
	if err := yaml.Unmarshal(out, &task); err != nil {
		return Task{}, err
	}
	return task, nil
}

func (c CLI) GetWorkspace(ctx context.Context, space, name string) (Workspace, error) {
	out, err := c.ax(ctx, nil, "get", "workspace", name, "--atespace", space)
	if err != nil {
		return Workspace{}, err
	}
	var workspace Workspace
	if err := yaml.Unmarshal(out, &workspace); err != nil {
		return Workspace{}, err
	}
	return workspace, nil
}

func (c CLI) GetGateway(ctx context.Context, space, name string) (Gateway, error) {
	out, err := c.ax(ctx, nil, "get", "gateway", name, "--atespace", space)
	if err != nil {
		return Gateway{}, err
	}
	var gateway Gateway
	if err := yaml.Unmarshal(out, &gateway); err != nil {
		return Gateway{}, err
	}
	return gateway, nil
}

func (c CLI) Apply(ctx context.Context, task Task) error {
	manifest, err := yaml.Marshal(struct {
		APIVersion string         `yaml:"apiVersion"`
		Kind       string         `yaml:"kind"`
		Metadata   TaskMetadata   `yaml:"metadata"`
		Spec       map[string]any `yaml:"spec"`
	}{task.APIVersion, task.Kind, task.Metadata, task.Spec})
	if err != nil {
		return err
	}
	_, err = c.ax(ctx, manifest, "apply", "-f", "-")
	return err
}

func (c CLI) Delete(ctx context.Context, space, name string) error {
	_, err := c.ax(ctx, nil, "delete", "task", name, "--atespace", space)
	return err
}

func (c CLI) ateCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	if c.AtePath == "" {
		return nil, errors.New("Substrate CLI is not configured")
	}
	cmdArgs := make([]string, 0, len(args)+4)
	cmdEnv := os.Environ()
	if c.AteEndpoint != "" || c.AteTokenFile != "" || c.AteCAFile != "" {
		host, port, err := net.SplitHostPort(c.AteEndpoint)
		portNumber, portErr := strconv.Atoi(port)
		if err != nil || host == "" || strings.ContainsAny(host, "/?#@\\ \t\r\n") ||
			portErr != nil || portNumber < 1 || portNumber > 65535 ||
			!filepath.IsAbs(c.AteTokenFile) || !filepath.IsAbs(c.AteCAFile) {
			return nil, errors.New("direct Substrate access requires a host:port endpoint and absolute token/CA files")
		}
		cmdArgs = append(cmdArgs, "--endpoint", c.AteEndpoint, "--token-file", c.AteTokenFile)
		cmdEnv = replaceEnv(cmdEnv, "KUBECTL_ATE_CA_FILE", c.AteCAFile)
	}
	cmdArgs = append(cmdArgs, args...)
	cmd := exec.CommandContext(ctx, c.AtePath, cmdArgs...) // #nosec G204 -- pinned Substrate executable with separate argv; no shell
	cmd.Env = cmdEnv
	return cmd, nil
}

func replaceEnv(env []string, key, value string) []string {
	result := env[:0]
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if name != key {
			result = append(result, entry)
		}
	}
	return append(result, key+"="+value)
}

func (c CLI) ate(ctx context.Context, args ...string) ([]byte, error) {
	cmd, err := c.ateCommand(ctx, args...)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && strings.Contains(string(exit.Stderr), "code = NotFound") {
		return nil, ErrNotFound
	}
	return nil, fmt.Errorf("Substrate read: %w", err)
}

// Current reads AX's underlying actor, template, and pool from Substrate.
// Neither the task manifest nor the guest selects these observed values.
func (c CLI) Current(ctx context.Context, space, name string) (bootstrap.Runtime, error) {
	data, err := c.ate(ctx, "get", "actor", name, "-a", space, "-o", "json")
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	var actors struct {
		Actors []struct {
			Metadata      struct{ Atespace, Name, UID string }
			ActorTemplate struct{ Atespace, Name string }
			Status        struct {
				State, CurrentActorTemplateUID string
				WorkerAssignment               struct{ WorkerPod, WorkerPodUID, WorkerPool string }
			}
		}
	}
	if err := json.Unmarshal(data, &actors); err != nil || len(actors.Actors) != 1 {
		return bootstrap.Runtime{}, ErrMismatch
	}
	a := actors.Actors[0]
	if a.Status.State != "ACTOR_STATE_RUNNING" || a.Metadata.Atespace != space || a.Metadata.Name != name ||
		a.ActorTemplate.Atespace == "" || a.ActorTemplate.Name == "" {
		return bootstrap.Runtime{}, ErrPending
	}
	data, err = c.ate(ctx, "get", "actor-template", a.ActorTemplate.Name, "-a", a.ActorTemplate.Atespace, "-o", "json")
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	var templates struct {
		ActorTemplates []struct {
			Metadata   struct{ UID string }
			Containers []struct {
				Image   string
				Command []string
				Env     []struct{ Name, Value string }
			}
			SandboxConfig   struct{ SandboxClass string }
			SnapshotsConfig struct {
				OnPause, OnCommit, StorageLocation string
				OnResume                           struct{ FromData string }
			}
		}
	}
	if err := json.Unmarshal(data, &templates); err != nil || len(templates.ActorTemplates) != 1 {
		return bootstrap.Runtime{}, ErrMismatch
	}
	t := templates.ActorTemplates[0]
	if a.Status.CurrentActorTemplateUID != t.Metadata.UID || len(t.Containers) != 1 ||
		len(t.Containers[0].Command) != 1 || t.Containers[0].Command[0] != "/usr/local/bin/ax-task-runner" {
		return bootstrap.Runtime{}, ErrMismatch
	}
	var signer string
	for _, item := range t.Containers[0].Env {
		if item.Name == "BLAXSMITH_BOOTSTRAP_PUBLIC_KEY" {
			if signer != "" {
				return bootstrap.Runtime{}, ErrMismatch
			}
			signer = item.Value
		}
	}
	return bootstrap.Runtime{Actor: bootstrap.Actor{Atespace: a.Metadata.Atespace, Name: a.Metadata.Name, UID: a.Metadata.UID},
		TemplateUID: t.Metadata.UID, Image: t.Containers[0].Image,
		SandboxClass: t.SandboxConfig.SandboxClass, BootstrapPublicKey: signer,
		WorkerPod: a.Status.WorkerAssignment.WorkerPod, WorkerPodUID: a.Status.WorkerAssignment.WorkerPodUID,
		WorkerPool:      a.Status.WorkerAssignment.WorkerPool,
		SnapshotOnPause: t.SnapshotsConfig.OnPause, SnapshotOnCommit: t.SnapshotsConfig.OnCommit,
		ResumeFromData: t.SnapshotsConfig.OnResume.FromData, SnapshotStorage: t.SnapshotsConfig.StorageLocation}, nil
}

// Gone confirms that the exact Substrate actor identity is absent after AX's
// two-phase delete. A failed control-plane read never counts as absence.
func (c CLI) Gone(ctx context.Context, space, name string) (bool, error) {
	out, err := c.ate(ctx, "get", "actor", name, "-a", space, "-o", "json")
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return true, nil
		}
		return false, err
	}
	var response struct {
		Actors []struct {
			Metadata struct{ Atespace, Name string }
		}
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return false, err
	}
	for _, actor := range response.Actors {
		if actor.Metadata.Atespace == space && actor.Metadata.Name == name {
			return false, nil
		}
	}
	return true, nil
}
