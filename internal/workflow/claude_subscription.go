package workflow

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// claudeSetupToken is the shape `claude setup-token` prints. Only the shape is
// checked: a subscription token can't be validated against the API-key models
// endpoint, and nothing here calls Anthropic with it.
var claudeSetupToken = regexp.MustCompile(`^sk-ant-oat[A-Za-z0-9_-]{8,500}$`)

// ClaudeSubscriptionAllowed reports whether members may use their own Claude
// subscription for their own runs. Any member may read it.
func (s *Store) ClaudeSubscriptionAllowed(ctx context.Context, caller identity.Caller) (bool, error) {
	if !ids(caller.OrganizationID) {
		return false, ErrInvalid
	}
	var allowed bool
	err := s.pool.QueryRow(ctx, `SELECT allow_member_claude_subscription FROM identity_organizations WHERE id::text=$1`,
		caller.OrganizationID).Scan(&allowed)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	return allowed, err
}

// SetClaudeSubscriptionAllowedAs flips the org switch. Owners and admins only;
// the live session and role are rechecked, and the change is audited.
func (s *Store) SetClaudeSubscriptionAllowedAs(ctx context.Context, caller identity.Caller, allowed bool) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrConnectionDenied
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockCallerSession(ctx, tx, caller, false); err != nil {
		return err
	}
	var before bool
	if err := tx.QueryRow(ctx, `SELECT allow_member_claude_subscription FROM identity_organizations WHERE id::text=$1 FOR UPDATE`,
		caller.OrganizationID).Scan(&before); err != nil {
		return err
	}
	if before == allowed {
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_organizations SET allow_member_claude_subscription=$2 WHERE id::text=$1`,
		caller.OrganizationID, allowed); err != nil {
		return err
	}
	action := "access.policy.claude_subscription_disabled"
	if allowed {
		action = "access.policy.claude_subscription_enabled"
	}
	if err := audit(ctx, tx, caller, action, caller.OrganizationID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func requireClaudeSubscriptionAllowed(ctx context.Context, tx pgx.Tx, orgID string) error {
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT allow_member_claude_subscription FROM identity_organizations WHERE id::text=$1 FOR SHARE`,
		orgID).Scan(&allowed); err != nil {
		return err
	}
	if !allowed {
		return access.ErrClaudeSubscriptionDisabled
	}
	return nil
}

// CreateClaudeSubscriptionAs stores the caller's own `claude setup-token` as a
// personal connection. It is used only through the owner's own project uses
// (user grants, native_raw), and access.deliveryAllowed keeps it owner-only.
func (s *Store) CreateClaudeSubscriptionAs(ctx context.Context, caller identity.Caller, token []byte,
	secrets *access.SecretStore) (Connection, error) {
	if secrets == nil || !claudeSetupToken.Match(token) {
		return Connection{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopePersonal, "")
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	if err := requireClaudeSubscriptionAllowed(ctx, tx, caller.OrganizationID); err != nil {
		return Connection{}, err
	}
	var providerID, connectionID string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,'anthropic',$2,ARRAY['native_raw'],'active') RETURNING id`,
		caller.OrganizationID, access.ModelOrigin("anthropic")).Scan(&providerID); err != nil {
		return Connection{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,label,models_checked_at)
		VALUES ($1,gen_random_uuid()::text,'user',$2,$3,'claude-subscription',$4,'active','Claude subscription',clock_timestamp())
		RETURNING id`, caller.OrganizationID, caller.PrincipalID, providerID, access.ClaudeSetupTokenAuth).Scan(&connectionID); err != nil {
		return Connection{}, err
	}
	if _, err := secrets.RotateTx(ctx, tx, caller.OrganizationID, connectionID, 0, token, nil); err != nil {
		return Connection{}, err
	}
	if err := audit(ctx, tx, caller, "access.subscription.connected", connectionID); err != nil {
		return Connection{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Connection{}, err
	}
	return s.connectionFor(ctx, caller, connectionID)
}
