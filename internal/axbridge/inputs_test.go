package axbridge

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"gopkg.in/yaml.v3"
)

type inputAX struct {
	fakeAX
	workspace Workspace
	gateway   Gateway
	err       error
}

func (a inputAX) GetWorkspace(context.Context, string, string) (Workspace, error) {
	return a.workspace, a.err
}
func (a inputAX) GetGateway(context.Context, string, string) (Gateway, error) {
	return a.gateway, a.err
}

func TestToolInputsRequireNarrowNativeWorkspaceAndGateway(t *testing.T) {
	const org = "11111111-1111-1111-1111-111111111111"
	space := Space(org)
	workspace := Workspace{APIVersion: "ax.io/v1alpha1", Kind: "Workspace",
		Metadata: TaskMetadata{Name: "source", Atespace: space}, Spec: map[string]any{"git": []any{}, "mcp": nil, "skills": nil}}
	gateway := Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway",
		Metadata: TaskMetadata{Name: "public-egress", Atespace: space}}
	gateway.Spec.Egress = &GatewayEgress{Allowlist: &GatewayAllowlist{Hosts: []GatewayHostRule{
		{Host: "140.82.114.3/32"}, {Host: "104.18.33.45/32"},
	}}}
	bridge := Bridge{AX: &inputAX{workspace: workspace, gateway: gateway}, Workspace: "source", Gateway: "public-egress",
		LookupIPv4: func(_ context.Context, host string) ([]netip.Addr, error) {
			switch host {
			case "github.com":
				return []netip.Addr{netip.MustParseAddr("140.82.114.3")}, nil
			case "api.openai.com":
				return []netip.Addr{netip.MustParseAddr("104.18.33.45")}, nil
			default:
				return nil, errors.New("unapproved hostname")
			}
		}}
	check := func(want bool) {
		t.Helper()
		err := bridge.CheckToolInputs(t.Context(), org, "https://github.com/owner/repo", "openai")
		if (err == nil) != want {
			t.Fatalf("AX input check = %v, want pass %t", err, want)
		}
	}
	check(true)
	ax := bridge.AX.(*inputAX)
	ax.workspace.Spec["git"] = []any{map[string]any{"repo": "https://example.com/other"}}
	check(false)
	ax.workspace.Spec["git"] = []any{}
	ax.gateway.Spec.Egress.Allowlist.Hosts[0].Host = "0.0.0.0/0"
	check(false)
	ax.gateway.Spec.Egress.Allowlist.Hosts[0].Host = "10.0.0.1/32"
	check(false)
	ax.gateway.Spec.Egress.Allowlist.Hosts[0].Host = "140.82.114.3/32"
	ax.gateway.Spec.Egress.Allowlist.Hosts = append(ax.gateway.Spec.Egress.Allowlist.Hosts, GatewayHostRule{Host: "8.8.8.8/32"})
	check(false)
	ax.gateway.Spec.Egress.Allowlist.Hosts = ax.gateway.Spec.Egress.Allowlist.Hosts[:2]
	ax.gateway.Metadata.Atespace = "other"
	check(false)
	ax.gateway.Metadata.Atespace = space
	bridge.LookupIPv4 = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("DNS unavailable") }
	check(false)
}

// OpenCode Go (https://opencode.ai/zen/go/v1) shares Zen's host, so an exact
// Gateway for an opencode-go attempt admits Git plus opencode.ai only.
func TestOpenCodeGoGatewayAllowsOnlyOpenCodeHost(t *testing.T) {
	const space = "blaxsmith-engineering"
	gateway := Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway", Metadata: TaskMetadata{Name: "public-egress", Atespace: space},
		Spec: GatewaySpec{Egress: &GatewayEgress{Allowlist: &GatewayAllowlist{Hosts: []GatewayHostRule{
			{Host: "140.82.114.3/32"}, {Host: "104.21.0.10/32"}}}}}}
	bridge := Bridge{GatewayEgressMode: "exact", LookupIPv4: func(_ context.Context, host string) ([]netip.Addr, error) {
		switch host {
		case "github.com":
			return []netip.Addr{netip.MustParseAddr("140.82.114.3")}, nil
		case "opencode.ai":
			return []netip.Addr{netip.MustParseAddr("104.21.0.10")}, nil
		case "api.openai.com":
			return []netip.Addr{netip.MustParseAddr("104.18.33.45")}, nil
		}
		return nil, errors.New("unapproved hostname")
	}}
	if providerHost("opencode-go") != "opencode.ai" || providerHost("opencode-go") != providerHost("opencode") {
		t.Fatalf("OpenCode Go egress host: %q", providerHost("opencode-go"))
	}
	for provider, want := range map[string]bool{"opencode-go": true, "opencode": true, "openai": false, "opencode-zen": false} {
		err := bridge.checkGateway(t.Context(), gateway, "public-egress", space, "https://github.com/owner/repo", bridge.modelEgressHost(provider))
		if (err == nil) != want {
			t.Fatalf("%s gateway check = %v, want pass %t", provider, err, want)
		}
	}
}

func TestOpenGatewayRequiresExplicitDevelopmentMode(t *testing.T) {
	const repo = "https://github.com/owner/repo"
	const space = "blaxsmith-engineering"
	gateway := Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway",
		Metadata: TaskMetadata{Name: "public-egress", Atespace: space},
		Spec:     GatewaySpec{Egress: &GatewayEgress{Allowlist: &GatewayAllowlist{Hosts: []GatewayHostRule{{Host: "*"}}}}}}
	bridge := Bridge{GatewayEgressMode: "open-dev"}
	if err := bridge.checkGateway(t.Context(), gateway, "public-egress", space, repo, "openai"); err != nil {
		t.Fatalf("explicit development open Gateway rejected: %v", err)
	}
	bridge.GatewayEgressMode = "exact"
	if err := bridge.checkGateway(t.Context(), gateway, "public-egress", space, repo, "openai"); !errors.Is(err, ErrInputs) {
		t.Fatalf("open Gateway accepted in exact mode: %v", err)
	}
	bridge.GatewayEgressMode = "open-dev"
	gateway.Spec.Egress.Allowlist.Hosts[0].Port = 443
	if err := bridge.checkGateway(t.Context(), gateway, "public-egress", space, repo, "openai"); !errors.Is(err, ErrInputs) {
		t.Fatalf("port-restricted wildcard unexpectedly accepted: %v", err)
	}
	gateway.Spec.Egress.Allowlist.Hosts = []GatewayHostRule{{Host: "*"}, {Host: "8.8.8.8/32"}}
	if err := bridge.checkGateway(t.Context(), gateway, "public-egress", space, repo, "openai"); !errors.Is(err, ErrInputs) {
		t.Fatalf("wildcard mixed with extra egress rules accepted: %v", err)
	}
}

func TestOpenDevGatewayDoesNotRelaxWorkspaceValidation(t *testing.T) {
	const org = "11111111-1111-1111-1111-111111111111"
	space := Space(org)
	workspace := Workspace{APIVersion: "ax.io/v1alpha1", Kind: "Workspace",
		Metadata: TaskMetadata{Name: "source", Atespace: space}, Spec: map[string]any{"git": []any{}, "mcp": nil, "skills": nil}}
	gateway := Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway",
		Metadata: TaskMetadata{Name: "public-egress", Atespace: space},
		Spec:     GatewaySpec{Egress: &GatewayEgress{Allowlist: &GatewayAllowlist{Hosts: []GatewayHostRule{{Host: "*"}}}}}}
	bridge := Bridge{AX: &inputAX{workspace: workspace, gateway: gateway}, Workspace: "source",
		Gateway: "public-egress", GatewayEgressMode: "open-dev"}
	if err := bridge.CheckToolInputs(t.Context(), org, "https://github.com/owner/repo", "openai"); err != nil {
		t.Fatal(err)
	}
	workspace.Spec["ambient"] = true
	bridge.AX = &inputAX{workspace: workspace, gateway: gateway}
	if err := bridge.CheckToolInputs(t.Context(), org, "https://github.com/owner/repo", "openai"); !errors.Is(err, ErrInputs) {
		t.Fatalf("open-dev mode bypassed Workspace checks: %v", err)
	}
}

func TestAXResourceYAMLReadback(t *testing.T) {
	var workspace Workspace
	if err := yaml.Unmarshal([]byte(`apiVersion: ax.io/v1alpha1
kind: Workspace
metadata:
  name: source
  atespace: blaxsmith-test
spec:
  git: []
  mcp: null
  skills: null
`), &workspace); err != nil || !emptyWorkspaceSpec(workspace.Spec) {
		t.Fatalf("empty native workspace readback: %+v, %v", workspace, err)
	}
	var gateway Gateway
	if err := yaml.Unmarshal([]byte(`apiVersion: ax.io/v1alpha1
kind: Gateway
metadata:
  name: public-egress
  atespace: blaxsmith-test
spec:
  listeners: []
  egress:
    allowlist:
      hosts:
        - host: 140.82.114.3/32
          port: 0
`), &gateway); err != nil || len(gateway.Spec.Egress.Allowlist.Hosts) != 1 {
		t.Fatalf("gateway readback: %+v, %v", gateway, err)
	}
}

func TestPerAttemptWorkspaceMatchesOnlyFrozenGitInput(t *testing.T) {
	request := tooladapter.Request{RepositoryURL: "https://github.com/owner/repo", SourceRef: "feature/work",
		SourceCommit:    "0123456789abcdef0123456789abcdef01234567",
		SourceDirectory: "source"}
	name := AttemptWorkspaceName("attempt-1234")
	workspace := sourceWorkspace("space", name, request)
	if name != "source-attempt1234" || !sourceWorkspaceMatches(workspace, "space", name, request) {
		t.Fatalf("valid attempt Workspace rejected: %+v", workspace)
	}
	git := workspace.Spec["git"].([]any)[0].(map[string]any)
	git["branch"] = "main"
	if sourceWorkspaceMatches(workspace, "space", name, request) {
		t.Fatal("Workspace not pinned to commit accepted")
	}
	git["branch"] = request.SourceCommit
	request.SourceCommit = "1123456789abcdef0123456789abcdef01234567"
	if sourceWorkspaceMatches(workspace, "space", name, request) {
		t.Fatal("Workspace for another frozen commit accepted")
	}
	request.SourceCommit = "0123456789abcdef0123456789abcdef01234567"
	workspace.Spec["mcp"] = map[string]any{"servers": []any{"ambient"}}
	if sourceWorkspaceMatches(workspace, "space", name, request) {
		t.Fatal("unapproved MCP configuration accepted")
	}
	request.SourceRef = ""
	delete(workspace.Spec, "mcp")
	if !sourceWorkspaceMatches(workspace, "space", name, request) {
		t.Fatal("empty ref changed the exact commit Workspace input")
	}
}

func TestPerAttemptGatewayCopiesOnlyApprovedEgress(t *testing.T) {
	template := Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway",
		Metadata: TaskMetadata{Name: "public-egress", Atespace: "shared"},
		Spec: GatewaySpec{Egress: &GatewayEgress{Allowlist: &GatewayAllowlist{Hosts: []GatewayHostRule{
			{Host: "140.82.114.3/32"}, {Host: "104.18.33.45/32"},
		}}}}}
	name := AttemptGatewayName("attempt-1234")
	got := attemptGateway(template, "space", name)
	if name != "egress-attempt1234" || got.Metadata != (TaskMetadata{Name: name, Atespace: "space"}) ||
		!attemptGatewayMatches(got, attemptGateway(template, "space", name)) {
		t.Fatalf("attempt Gateway identity/config: %+v", got)
	}
	got.Spec.Egress.Allowlist.Hosts[0].Host = "0.0.0.0/0"
	if attemptGatewayMatches(got, attemptGateway(template, "space", name)) {
		t.Fatal("mutated attempt Gateway matched the approved template")
	}
}
