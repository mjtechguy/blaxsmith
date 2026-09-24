package dispatch

import (
	"context"
	"net/url"

	"github.com/mjtechguy/blaxsmith/internal/gateway"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

// gatewayDelivery picks a new attempt's delivery mode (§15.1). Without a
// configured gateway every attempt stays native_raw.
func (d *Dispatcher) gatewayDelivery(ctx context.Context, orgID, projectID string) (gateway.Delivery, error) {
	if d.GatewayURL == "" {
		return gateway.Delivery{Mode: gateway.ModeNative}, nil
	}
	return gateway.EffectiveDelivery(ctx, d.DB, orgID, projectID, d.GatewayURL)
}

func gatewayRequest(delivery gateway.Delivery) *tooladapter.Gateway {
	if delivery.Mode != gateway.ModeBrokered {
		return nil
	}
	return &tooladapter.Gateway{BaseURL: delivery.BaseURL}
}

func gatewayEgressHost(delivery gateway.Delivery) string {
	if delivery.Mode != gateway.ModeBrokered || !delivery.RemoveDirectEgress {
		return ""
	}
	u, err := url.Parse(delivery.BaseURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
