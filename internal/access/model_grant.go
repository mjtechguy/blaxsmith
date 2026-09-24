package access

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelGrant identifies a current grant before the scheduler reserves an
// attempt. It contains no secret and cannot authorize delivery by itself.
type ModelGrant struct {
	OrganizationID string
	ProjectID      string
	GrantID        string
	GranteeKind    string
	GranteeID      string
	Provider       string
	Model          string
}

type ModelApproval struct {
	Decision      Decision
	GrantVersion  int64
	PolicyVersion int64
	SecretVersion int64
}

// PreflightModelInvoke checks current authority and secret presence before a
// workflow attempt is reserved. BindModelInvoke repeats it after reservation;
// bootstrap release repeats it again against the frozen binding.
func PreflightModelInvoke(ctx context.Context, tx pgx.Tx, request ModelGrant) (ModelApproval, error) {
	if tx == nil || request.OrganizationID == "" || request.ProjectID == "" || request.GrantID == "" ||
		request.GranteeKind == "" || request.GranteeID == "" ||
		modelOrigin(request.Provider) == "" || !modelName.MatchString(request.Model) {
		return ModelApproval{}, ErrDenied
	}
	var connectionID, projectID, granteeKind, granteeID, capability, resource, deliveryMode string
	var connectionState, providerID, externalAccountID, providerKind, providerOrigin, providerState string
	var grantVersion, policyVersion, secretVersion int64
	var policyModes, providerModes []string
	var grantExpiry, secretExpiry, revokedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT g.connection_id,g.project_id,g.grantee_kind,g.grantee_id,g.capability,g.resource,
		g.delivery_mode,g.version,g.expires_at,g.revoked_at,
		p.version,p.delivery_modes,c.state,c.provider_registration_id,c.external_account_id,c.active_secret_version,
		s.expires_at,r.provider_kind,r.origin,r.delivery_modes,r.state
		FROM access_grants g
		JOIN access_project_policies p ON p.organization_id=g.organization_id AND p.project_id=g.project_id
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		JOIN access_secret_versions s ON s.organization_id=c.organization_id AND s.connection_id=c.id
			AND s.version=c.active_secret_version
		JOIN access_provider_registrations r ON r.organization_id=g.organization_id AND r.id=c.provider_registration_id
		WHERE g.organization_id=$1 AND g.id=$2 AND g.project_id=$3
		FOR SHARE OF g,p,c,s,r`, request.OrganizationID, request.GrantID, request.ProjectID).Scan(
		&connectionID, &projectID, &granteeKind, &granteeID, &capability, &resource,
		&deliveryMode, &grantVersion, &grantExpiry, &revokedAt,
		&policyVersion, &policyModes, &connectionState, &providerID, &externalAccountID, &secretVersion,
		&secretExpiry, &providerKind, &providerOrigin, &providerModes, &providerState)
	if err != nil {
		return ModelApproval{}, deniedOrError("model grant", err)
	}
	if projectID != request.ProjectID || granteeKind != request.GranteeKind || granteeID != request.GranteeID ||
		capability != "model.invoke" || resource != request.Provider+"/"+request.Model ||
		deliveryMode != "native_raw" || !slices.Contains(policyModes, deliveryMode) ||
		!slices.Contains(providerModes, deliveryMode) || revokedAt != nil || connectionState != "active" ||
		providerState != "active" || providerKind != request.Provider || providerOrigin != modelOrigin(request.Provider) {
		return ModelApproval{}, ErrDenied
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return ModelApproval{}, fmt.Errorf("read model grant clock: %w", err)
	}
	if (grantExpiry != nil && !grantExpiry.After(now)) || (secretExpiry != nil && !secretExpiry.After(now)) {
		return ModelApproval{}, ErrDenied
	}
	return ModelApproval{Decision: Decision{ConnectionID: connectionID, ProviderID: providerID,
		ExternalAccountID: externalAccountID, GrantID: request.GrantID, DeliveryMode: deliveryMode},
		GrantVersion: grantVersion, PolicyVersion: policyVersion, SecretVersion: secretVersion}, nil
}

// BindModelInvoke snapshots the versions observed by preflight for one
// reserved attempt. A changed grant, policy, connection, or secret denies.
func BindModelInvoke(ctx context.Context, tx pgx.Tx, request ModelGrant, approval ModelApproval, attemptID string) (string, error) {
	if attemptID == "" || approval.GrantVersion <= 0 || approval.PolicyVersion <= 0 || approval.SecretVersion <= 0 {
		return "", ErrDenied
	}
	current, err := PreflightModelInvoke(ctx, tx, request)
	if err != nil || current.GrantVersion != approval.GrantVersion ||
		current.PolicyVersion != approval.PolicyVersion || current.SecretVersion != approval.SecretVersion ||
		current.Decision.ConnectionID != approval.Decision.ConnectionID {
		return "", ErrDenied
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	bindingID := base64.RawURLEncoding.EncodeToString(id[:])
	err = tx.QueryRow(ctx, `INSERT INTO access_bindings
		(organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,policy_version)
		VALUES ($1,$2,$3,$4,$5,$6,'model.invoke',$7,$8) RETURNING id`,
		request.OrganizationID, bindingID, attemptID, request.ProjectID, request.GrantID,
		approval.GrantVersion, request.Provider+"/"+request.Model, approval.PolicyVersion).Scan(&bindingID)
	if err != nil {
		return "", fmt.Errorf("bind model grant: %w", err)
	}
	return bindingID, nil
}
