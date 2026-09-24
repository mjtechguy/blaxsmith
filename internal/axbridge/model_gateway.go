package axbridge

import (
	"context"
	"net/netip"
	"sort"
)

// modelEgressHost is the one model host an attempt's egress allows: the
// provider's, or the Blaxsmith model gateway's when a brokered_gateway
// attempt has direct provider egress removed (docs/model-gateway-plan.md §2).
func (b *Bridge) modelEgressHost(provider string) string {
	if b.ModelGatewayHost != "" {
		return b.ModelGatewayHost
	}
	return providerHost(provider)
}

// attemptGatewayFor derives the attempt Gateway from the validated template.
// With the model gateway replacing provider egress (exact mode only), the
// allowlist is rebuilt as the Git host plus the gateway host, so the
// sandbox cannot reach the provider directly. Open-dev egress cannot be
// narrowed; it stays as configured.
func (b *Bridge) attemptGatewayFor(ctx context.Context, template Gateway, space, name, provider string) (Gateway, error) {
	gateway := attemptGateway(template, space, name)
	if b.ModelGatewayHost == "" || b.GatewayEgressMode == "open-dev" {
		return gateway, nil
	}
	expected, err := b.exactEgress(ctx, b.Tool.RepositoryURL, b.ModelGatewayHost)
	if err != nil {
		return Gateway{}, err
	}
	addresses := make([]netip.Addr, 0, len(expected))
	for address := range expected {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i].Less(addresses[j]) })
	hosts := make([]GatewayHostRule, 0, len(addresses))
	for _, address := range addresses {
		hosts = append(hosts, GatewayHostRule{Host: netip.PrefixFrom(address, 32).String()})
	}
	gateway.Spec.Egress.Allowlist.Hosts = hosts
	return gateway, nil
}
