package axbridge

import (
	"context"
	"errors"
	"net/netip"
	"testing"

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
