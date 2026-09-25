package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Delivery is how one attempt receives model access.
type Delivery struct {
	Mode    string // native_raw or brokered_gateway
	BaseURL string // gateway origin the sandbox calls; empty for native_raw
	// RemoveDirectEgress drops the provider host from the attempt's AX
	// egress allowlist (§15.1); only meaningful for brokered_gateway.
	RemoveDirectEgress bool
}

// ValidBaseURL accepts the gateway origin the app is configured with: an
// absolute http(s) URL with no path, credentials, query, or fragment.
func ValidBaseURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == "" && len(raw) <= 256 &&
		!strings.ContainsAny(raw, " \r\n\x00")
}

// EffectiveDelivery decides a new attempt's mode from the organization
// switches and the project choice (§15.1). With the master switch off every
// project falls back to native_raw on its next attempt; baseURL is the
// installation's gateway ("" when the Helm gateway Deployment does not exist).
func EffectiveDelivery(ctx context.Context, db *pgxpool.Pool, orgID, projectID, baseURL string) (Delivery, error) {
	ctx = tenant.Org(ctx, orgID)
	if db == nil || orgID == "" || projectID == "" {
		return Delivery{}, ErrDenied
	}
	var enabled, allowChoice, removeEgress bool
	var defaultMode string
	var projectMode *string
	err := db.QueryRow(ctx, `SELECT s.enabled,s.allow_project_choice,s.remove_direct_egress,s.default_delivery_mode,p.delivery_mode
		FROM gateway_org_settings s
		LEFT JOIN gateway_project_settings p ON p.organization_id=s.organization_id AND p.project_id=$2
		WHERE s.organization_id=$1`, orgID, projectID).Scan(&enabled, &allowChoice, &removeEgress, &defaultMode, &projectMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{Mode: ModeNative}, nil
	}
	if err != nil {
		return Delivery{}, fmt.Errorf("read gateway settings: %w", err)
	}
	mode := defaultMode
	if allowChoice && projectMode != nil {
		mode = *projectMode
	}
	if !enabled || mode != ModeBrokered || !ValidBaseURL(baseURL) {
		return Delivery{Mode: ModeNative}, nil
	}
	return Delivery{Mode: ModeBrokered, BaseURL: strings.TrimSuffix(baseURL, "/"), RemoveDirectEgress: removeEgress}, nil
}

// RecordAttemptDelivery freezes the attempt's mode in its reservation
// transaction; completion and recovery rebuild the same tool command from it.
func RecordAttemptDelivery(ctx context.Context, tx pgx.Tx, orgID, attemptID, harness string, delivery Delivery) error {
	if tx == nil || orgID == "" || attemptID == "" || (delivery.Mode != ModeNative && delivery.Mode != ModeBrokered) ||
		(delivery.Mode == ModeBrokered) != (delivery.BaseURL != "") {
		return ErrDenied
	}
	_, err := tx.Exec(ctx, `INSERT INTO gateway_attempt_delivery (organization_id,attempt_id,delivery_mode,base_url,remove_direct_egress,harness)
		VALUES ($1,$2,$3,$4,$5,$6)`, orgID, attemptID, delivery.Mode, delivery.BaseURL,
		delivery.Mode == ModeBrokered && delivery.RemoveDirectEgress, harness)
	return err
}

// AttemptDelivery reads the frozen mode; attempts dispatched before G1 have
// no row and are native_raw.
func AttemptDelivery(ctx context.Context, db *pgxpool.Pool, orgID, attemptID string) (Delivery, error) {
	ctx = tenant.Org(ctx, orgID)
	var delivery Delivery
	err := db.QueryRow(ctx, `SELECT delivery_mode,base_url,remove_direct_egress FROM gateway_attempt_delivery
		WHERE organization_id=$1 AND attempt_id=$2`, orgID, attemptID).Scan(&delivery.Mode, &delivery.BaseURL, &delivery.RemoveDirectEgress)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{Mode: ModeNative}, nil
	}
	return delivery, err
}
