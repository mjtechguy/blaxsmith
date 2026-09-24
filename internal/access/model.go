package access

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ModelInvoke is a trusted scheduler request for one exact provider/model.
// A connection alone grants nothing; the frozen binding and current grant,
// project policy, provider registration, and secret all have to agree.
type ModelInvoke struct {
	OrganizationID string
	ProjectID      string
	AttemptID      string
	BindingID      string
	GranteeKind    string
	GranteeID      string
	Provider       string
	Model          string
	PolicyVersion  int64
}

func (r ModelInvoke) resource() string { return r.Provider + "/" + r.Model }

// ModelOrigin is the approved endpoint for a model provider, or empty.
func ModelOrigin(provider string) string { return modelOrigin(provider) }

func modelOrigin(provider string) string {
	switch provider {
	case "openai":
		return "https://api.openai.com"
	case "anthropic":
		return "https://api.anthropic.com"
	case "opencode": // OpenCode Zen, pay-as-you-go.
		return "https://opencode.ai/zen/v1"
	case "opencode-go": // OpenCode Go subscription, also an API key.
		return "https://opencode.ai/zen/go/v1"
	default:
		return ""
	}
}

// AuthorizeModelInvoke holds share locks on every revocable authority row
// through the caller's bootstrap send transaction. Raw API-key delivery and
// owner-only Codex subscription access (oauth_access) are implemented;
// brokered and short-lived modes fail closed.
func AuthorizeModelInvoke(ctx context.Context, tx pgx.Tx, request ModelInvoke) (Decision, error) {
	if tx == nil || request.OrganizationID == "" || request.ProjectID == "" || request.AttemptID == "" ||
		request.BindingID == "" || request.GranteeKind == "" || request.GranteeID == "" ||
		request.PolicyVersion <= 0 || modelOrigin(request.Provider) == "" || !modelName.MatchString(request.Model) {
		return Decision{}, ErrDenied
	}
	var projectID, capability, resource string
	var bindingGrantID, grantID, connectionID, grantProject, granteeKind, granteeID string
	var grantCapability, grantResource, deliveryMode, connectionState, providerID string
	var providerKind, providerOrigin, providerState, externalAccountID string
	var ownerKind, ownerID, authMethod string
	var grantVersion, currentGrantVersion, policyVersion, currentPolicyVersion int64
	var inputCommit *string
	var policyModes, providerModes []string
	var grantExpiresAt, revokedAt *time.Time
	err := tx.QueryRow(ctx, `SELECT b.project_id,b.capability,b.resource,b.input_commit,b.grant_id,b.grant_version,b.policy_version,
		p.version,p.delivery_modes,g.id,g.connection_id,g.project_id,g.grantee_kind,g.grantee_id,
		g.capability,g.resource,g.delivery_mode,g.version,g.expires_at,g.revoked_at,
		c.state,c.provider_registration_id,c.external_account_id,c.owner_kind,c.owner_id,c.auth_method,
		r.provider_kind,r.origin,r.delivery_modes,r.state
		FROM access_bindings b
		JOIN access_project_policies p ON p.organization_id=b.organization_id AND p.project_id=b.project_id
		JOIN access_grants g ON g.organization_id=b.organization_id AND g.id=b.grant_id
		JOIN access_connections c ON c.organization_id=b.organization_id AND c.id=g.connection_id
		JOIN access_provider_registrations r ON r.organization_id=b.organization_id AND r.id=c.provider_registration_id
		WHERE b.organization_id=$1 AND b.id=$2 AND b.attempt_id=$3
		FOR SHARE OF b,p,g,c,r`, request.OrganizationID, request.BindingID, request.AttemptID).Scan(
		&projectID, &capability, &resource, &inputCommit, &bindingGrantID, &grantVersion, &policyVersion,
		&currentPolicyVersion, &policyModes, &grantID, &connectionID, &grantProject, &granteeKind, &granteeID,
		&grantCapability, &grantResource, &deliveryMode, &currentGrantVersion, &grantExpiresAt, &revokedAt,
		&connectionState, &providerID, &externalAccountID, &ownerKind, &ownerID, &authMethod,
		&providerKind, &providerOrigin, &providerModes, &providerState)
	if err != nil {
		return Decision{}, deniedOrError("model authority", err)
	}
	if projectID != request.ProjectID || capability != "model.invoke" || resource != request.resource() ||
		inputCommit != nil || bindingGrantID != grantID || grantVersion != currentGrantVersion ||
		policyVersion != request.PolicyVersion || currentPolicyVersion != policyVersion ||
		grantProject != projectID || granteeKind != request.GranteeKind || granteeID != request.GranteeID ||
		grantCapability != capability || grantResource != resource ||
		!deliveryAllowed(deliveryMode, authMethod, request.Provider, ownerKind, ownerID, granteeKind, granteeID) ||
		!slices.Contains(policyModes, deliveryMode) || !slices.Contains(providerModes, deliveryMode) ||
		revokedAt != nil || connectionState != "active" || providerState != "active" ||
		providerKind != request.Provider || providerOrigin != modelOrigin(request.Provider) {
		return Decision{}, ErrDenied
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Decision{}, fmt.Errorf("read model policy clock: %w", err)
	}
	if grantExpiresAt != nil && !grantExpiresAt.After(now) {
		return Decision{}, ErrDenied
	}
	return Decision{ConnectionID: connectionID, ProviderID: providerID, ExternalAccountID: externalAccountID,
		GrantID: grantID, BindingID: request.BindingID, DeliveryMode: deliveryMode}, nil
}

// deliveryAllowed keeps each credential shape on its own delivery path. A
// personal subscription login (plan §10.3) is usable only by attempts whose
// grantee is the connection owner, never a workload, team, or another user.
func deliveryAllowed(mode, authMethod, provider, ownerKind, ownerID, granteeKind, granteeID string) bool {
	switch mode {
	case "native_raw":
		return authMethod != CodexSubscriptionAuth
	case "oauth_access":
		return authMethod == CodexSubscriptionAuth && provider == "openai" &&
			ownerKind == "user" && granteeKind == "user" && granteeID == ownerID
	}
	return false
}
