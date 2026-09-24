package axbridge

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

var ErrInputs = errors.New("approved AX workspace or gateway unavailable")

var axResourceName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

type Workspace struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   TaskMetadata   `yaml:"metadata"`
	Spec       map[string]any `yaml:"spec"`
}

type Gateway struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   TaskMetadata `yaml:"metadata"`
	Spec       GatewaySpec  `yaml:"spec"`
}

type GatewaySpec struct {
	Listeners []any          `yaml:"listeners"`
	Egress    *GatewayEgress `yaml:"egress"`
	Other     map[string]any `yaml:",inline"`
}

type GatewayEgress struct {
	Allowlist *GatewayAllowlist `yaml:"allowlist"`
	Other     map[string]any    `yaml:",inline"`
}

type GatewayAllowlist struct {
	Hosts []GatewayHostRule `yaml:"hosts"`
	Other map[string]any    `yaml:",inline"`
}

type GatewayHostRule struct {
	Host  string         `yaml:"host"`
	Port  int            `yaml:"port"`
	Other map[string]any `yaml:",inline"`
}

// InputReader reads the exact AX resources the controller binds to a Task.
type InputReader interface {
	GetWorkspace(context.Context, string, string) (Workspace, error)
	GetGateway(context.Context, string, string) (Gateway, error)
}

type WorkspaceManager interface {
	InputReader
	ApplyWorkspace(context.Context, Workspace) error
	DeleteWorkspace(context.Context, string, string) error
}

func AttemptWorkspaceName(attemptID string) string {
	name := "source-" + strings.ReplaceAll(attemptID, "-", "")
	if !axResourceName.MatchString(name) {
		return ""
	}
	return name
}

func sourceWorkspace(space, name string, request tooladapter.Request) Workspace {
	return Workspace{APIVersion: "ax.io/v1alpha1", Kind: "Workspace",
		Metadata: TaskMetadata{Name: name, Atespace: space}, Spec: map[string]any{"git": []any{
			map[string]any{"name": "source", "repo": request.RepositoryURL, "branch": request.SourceCommit,
				"dir": request.SourceDirectory, "depth": 1},
		}}}
}

func sourceWorkspaceMatches(workspace Workspace, space, name string, request tooladapter.Request) bool {
	if gitfetch.Validate(request.RepositoryURL, request.SourceRef) != nil || !gitfetch.IsCommit(request.SourceCommit) ||
		workspace.APIVersion != "ax.io/v1alpha1" || workspace.Kind != "Workspace" ||
		workspace.Metadata != (TaskMetadata{Name: name, Atespace: space}) {
		return false
	}
	for key, value := range workspace.Spec {
		if key == "mcp" || key == "skills" {
			if value == nil {
				continue
			}
		}
		if key != "git" {
			return false
		}
	}
	git, ok := workspace.Spec["git"].([]any)
	if !ok || len(git) != 1 {
		return false
	}
	repo, ok := git[0].(map[string]any)
	if !ok || len(repo) != 5 || repo["name"] != "source" || repo["repo"] != request.RepositoryURL ||
		repo["branch"] != request.SourceCommit || repo["dir"] != request.SourceDirectory {
		return false
	}
	switch depth := repo["depth"].(type) {
	case int:
		return depth == 1
	case int32:
		return depth == 1
	case int64:
		return depth == 1
	case uint64:
		return depth == 1
	default:
		return false
	}
}

// CheckToolInputs validates the preflight Workspace or exact per-attempt Git
// Workspace and narrow CIDR Gateway. This is configuration inspection, not a
// dataplane measurement.
func (b *Bridge) CheckToolInputs(ctx context.Context, organizationID, repositoryURL, provider string) error {
	if b == nil || !axResourceName.MatchString(b.Workspace) || !axResourceName.MatchString(b.Gateway) {
		return ErrInputs
	}
	reader, ok := b.AX.(InputReader)
	if !ok || gitfetch.Validate(repositoryURL, "") != nil {
		return ErrInputs
	}
	source, err := url.Parse(repositoryURL)
	if err != nil {
		return ErrInputs
	}
	providerHost := ""
	switch provider {
	case "openai":
		providerHost = "api.openai.com"
	case "anthropic":
		providerHost = "api.anthropic.com"
	default:
		return ErrInputs
	}
	space := Space(organizationID)
	workspace, err := reader.GetWorkspace(ctx, space, b.Workspace)
	workspaceOK := err == nil && workspace.APIVersion == "ax.io/v1alpha1" && workspace.Kind == "Workspace" &&
		workspace.Metadata.Name == b.Workspace && workspace.Metadata.Atespace == space
	if workspaceOK && b.Tool != nil && b.Tool.SourceDirectory != "" {
		workspaceOK = sourceWorkspaceMatches(workspace, space, b.Workspace, *b.Tool)
	} else if workspaceOK {
		workspaceOK = emptyWorkspaceSpec(workspace.Spec)
	}
	if !workspaceOK {
		return ErrInputs
	}
	gateway, err := reader.GetGateway(ctx, space, b.Gateway)
	if err != nil || gateway.APIVersion != "ax.io/v1alpha1" || gateway.Kind != "Gateway" ||
		gateway.Metadata.Name != b.Gateway || gateway.Metadata.Atespace != space ||
		len(gateway.Spec.Listeners) != 0 || len(gateway.Spec.Other) != 0 ||
		gateway.Spec.Egress == nil || len(gateway.Spec.Egress.Other) != 0 ||
		gateway.Spec.Egress.Allowlist == nil || len(gateway.Spec.Egress.Allowlist.Other) != 0 {
		return ErrInputs
	}
	lookup := b.LookupIPv4
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		}
	}
	expected := map[netip.Addr]bool{}
	for _, host := range []string{source.Hostname(), providerHost} {
		addresses, err := lookup(ctx, host)
		if err != nil || len(addresses) == 0 {
			return ErrInputs
		}
		for _, address := range addresses {
			address = address.Unmap()
			if !gitfetch.PublicIPv4(address) {
				return ErrInputs
			}
			expected[address] = true
		}
	}
	hosts := gateway.Spec.Egress.Allowlist.Hosts
	if len(hosts) != len(expected) {
		return ErrInputs
	}
	for _, rule := range hosts {
		prefix, err := netip.ParsePrefix(rule.Host)
		if err != nil || rule.Port != 0 || len(rule.Other) != 0 || prefix.Bits() != 32 || prefix.Addr() != prefix.Masked().Addr() ||
			!expected[prefix.Addr()] {
			return ErrInputs
		}
		delete(expected, prefix.Addr())
	}
	if len(expected) != 0 {
		return ErrInputs
	}
	return nil
}

func emptyWorkspaceSpec(spec map[string]any) bool {
	for key, value := range spec {
		switch key {
		case "git":
			if value == nil {
				continue
			}
			git, ok := value.([]any)
			if ok && len(git) == 0 {
				continue
			}
		case "mcp", "skills":
			if value == nil {
				continue
			}
		}
		return false
	}
	return true
}
