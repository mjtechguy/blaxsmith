package axbridge

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

func TestModelGatewayReplacesProviderEgress(t *testing.T) {
	addrs := map[string][]netip.Addr{
		"github.com":        {netip.MustParseAddr("140.82.112.3")},
		"api.anthropic.com": {netip.MustParseAddr("160.79.104.10")},
		"gw.blaxsmith.dev":  {netip.MustParseAddr("34.117.59.81")},
	}
	b := &Bridge{Tool: &tooladapter.Request{RepositoryURL: "https://github.com/owner/repo.git"},
		LookupIPv4: func(_ context.Context, host string) ([]netip.Addr, error) {
			if a, ok := addrs[host]; ok {
				return a, nil
			}
			return nil, errors.New("no such host")
		}}
	template := Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway", Metadata: TaskMetadata{Name: "tpl", Atespace: "space"},
		Spec: GatewaySpec{Egress: &GatewayEgress{Allowlist: &GatewayAllowlist{Hosts: []GatewayHostRule{
			{Host: "140.82.112.3/32"}, {Host: "160.79.104.10/32"}}}}}}
	if err := b.checkGateway(tenant.System(t.Context()), template, "tpl", "space", b.Tool.RepositoryURL, providerHost("anthropic")); err != nil {
		t.Fatalf("template: %v", err)
	}
	// native_raw: the attempt keeps the template's provider egress.
	native, err := b.attemptGatewayFor(tenant.System(t.Context()), template, "space", "egress-a", "anthropic")
	if err != nil || len(native.Spec.Egress.Allowlist.Hosts) != 2 || native.Spec.Egress.Allowlist.Hosts[1].Host != "160.79.104.10/32" {
		t.Fatalf("native egress: %+v %v", native.Spec.Egress.Allowlist.Hosts, err)
	}
	// brokered with direct egress removed: Git plus the gateway, no provider.
	b.ModelGatewayHost = "gw.blaxsmith.dev"
	brokered, err := b.attemptGatewayFor(tenant.System(t.Context()), template, "space", "egress-a", "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	hosts := brokered.Spec.Egress.Allowlist.Hosts
	if len(hosts) != 2 || hosts[0].Host != "34.117.59.81/32" || hosts[1].Host != "140.82.112.3/32" {
		t.Fatalf("brokered egress: %+v", hosts)
	}
	b.Gateway = "egress-a"
	if err := b.checkGateway(tenant.System(t.Context()), brokered, "egress-a", "space", b.Tool.RepositoryURL, b.modelEgressHost("anthropic")); err != nil {
		t.Fatalf("re-check before release: %v", err)
	}
	if err := b.checkGateway(tenant.System(t.Context()), native, "egress-a", "space", b.Tool.RepositoryURL, b.modelEgressHost("anthropic")); !errors.Is(err, ErrInputs) {
		t.Fatalf("provider egress accepted for a brokered attempt: %v", err)
	}
	// A private gateway address cannot be an exact public egress rule.
	addrs["gw.blaxsmith.dev"] = []netip.Addr{netip.MustParseAddr("10.0.0.5")}
	if _, err := b.attemptGatewayFor(tenant.System(t.Context()), template, "space", "egress-a", "anthropic"); !errors.Is(err, ErrInputs) {
		t.Fatalf("private gateway address accepted: %v", err)
	}
}
