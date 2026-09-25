package access

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var ErrDenied = errors.New("access denied")

// GitRead is a trusted scheduler request, not a worker-selected scope. The
// caller supplies the current project policy version and frozen input commit.
type GitRead struct {
	OrganizationID string
	ProjectID      string
	AttemptID      string
	BindingID      string
	GranteeKind    string
	GranteeID      string
	RepoURL        string
	Commit         string
	PolicyVersion  int64
}

// Decision contains non-secret account and delivery metadata. It is not a
// provider credential or an access lease.
type Decision struct {
	ConnectionID      string
	ProviderID        string
	ExternalAccountID string
	GrantID           string
	BindingID         string
	DeliveryMode      string
	// BaseURL is the API-key connection's model endpoint (LiteLLM, a company
	// gateway); empty means the provider's own. Not secret.
	BaseURL string
}

// AuthorizeGitRead locks the frozen binding, current grant, and connection in
// one transaction. Call it from the bootstrap release transaction; revocation
// updates must touch the same rows so they cannot commit during delivery.
func AuthorizeGitRead(ctx context.Context, tx pgx.Tx, request GitRead) (Decision, error) {
	if tx == nil || request.OrganizationID == "" || request.ProjectID == "" || request.AttemptID == "" ||
		request.BindingID == "" || request.GranteeKind == "" || request.GranteeID == "" ||
		request.RepoURL == "" || request.PolicyVersion <= 0 || !validCommit(request.Commit) {
		return Decision{}, ErrDenied
	}
	u, err := url.Parse(request.RepoURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path == "" || u.Path == "/" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return Decision{}, ErrDenied
	}
	var grantID, projectID, capability, resource, commit string
	var grantVersion, policyVersion int64
	err = tx.QueryRow(ctx, `SELECT grant_id, grant_version, project_id, capability, resource,
		input_commit, policy_version FROM access_bindings
		WHERE organization_id=$1 AND id=$2 AND attempt_id=$3
		AND capability='git.read' AND input_commit=$4 FOR SHARE`,
		request.OrganizationID, request.BindingID, request.AttemptID, request.Commit).
		Scan(&grantID, &grantVersion, &projectID, &capability, &resource, &commit, &policyVersion)
	if err != nil {
		return Decision{}, deniedOrError("binding", err)
	}
	if projectID != request.ProjectID || capability != "git.read" || resource != request.RepoURL ||
		commit != request.Commit || policyVersion != request.PolicyVersion {
		return Decision{}, ErrDenied
	}
	var currentPolicyVersion int64
	var gitEnabled bool
	var projectModes []string
	err = tx.QueryRow(ctx, `SELECT version, git_read_enabled, delivery_modes FROM access_project_policies
		WHERE organization_id=$1 AND project_id=$2 FOR SHARE`, request.OrganizationID, request.ProjectID).
		Scan(&currentPolicyVersion, &gitEnabled, &projectModes)
	if err != nil {
		return Decision{}, deniedOrError("project policy", err)
	}
	if currentPolicyVersion != policyVersion || !gitEnabled {
		return Decision{}, ErrDenied
	}
	var connectionID, grantProject, granteeKind, granteeID, grantCapability, grantResource, deliveryMode string
	var currentVersion int64
	var expiresAt, revokedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT connection_id, project_id, grantee_kind, grantee_id,
		capability, resource, delivery_mode, version, expires_at, revoked_at
		FROM access_grants WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		request.OrganizationID, grantID).
		Scan(&connectionID, &grantProject, &granteeKind, &granteeID,
			&grantCapability, &grantResource, &deliveryMode, &currentVersion, &expiresAt, &revokedAt)
	if err != nil {
		return Decision{}, deniedOrError("grant", err)
	}
	if grantProject != request.ProjectID || granteeKind != request.GranteeKind ||
		granteeID != request.GranteeID || grantCapability != "git.read" ||
		grantResource != request.RepoURL || currentVersion != grantVersion || revokedAt != nil ||
		!slices.Contains(projectModes, deliveryMode) {
		return Decision{}, ErrDenied
	}
	var state, account, providerID string
	err = tx.QueryRow(ctx, `SELECT state, external_account_id, provider_registration_id FROM access_connections
		WHERE organization_id=$1 AND id=$2 FOR SHARE`, request.OrganizationID, connectionID).
		Scan(&state, &account, &providerID)
	if err != nil {
		return Decision{}, deniedOrError("connection", err)
	}
	if state != "active" {
		return Decision{}, ErrDenied
	}
	var kind, origin, providerState string
	var modes []string
	err = tx.QueryRow(ctx, `SELECT provider_kind, origin, delivery_modes, state
		FROM access_provider_registrations WHERE organization_id=$1 AND id=$2 FOR SHARE`,
		request.OrganizationID, providerID).Scan(&kind, &origin, &modes, &providerState)
	if err != nil {
		return Decision{}, deniedOrError("provider registration", err)
	}
	if kind != "git" || providerState != "active" || origin != "https://"+strings.ToLower(u.Host) ||
		!slices.Contains(modes, deliveryMode) {
		return Decision{}, ErrDenied
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Decision{}, fmt.Errorf("read policy clock: %w", err)
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return Decision{}, ErrDenied
	}
	return Decision{ConnectionID: connectionID, ProviderID: providerID, ExternalAccountID: account,
		GrantID: grantID, BindingID: request.BindingID, DeliveryMode: deliveryMode}, nil
}

func validCommit(commit string) bool {
	if len(commit) != 40 && len(commit) != 64 {
		return false
	}
	_, err := hex.DecodeString(commit)
	return err == nil && commit == strings.ToLower(commit)
}

func deniedOrError(object string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return fmt.Errorf("read %s authority: %w", object, err)
}
