package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
)

// Grant is one authorized request: the token's bound context plus the route
// credential. Key must be cleared by the caller once the request is sent.
type Grant struct {
	TokenID, OrganizationID, ProjectID, RunID, AttemptID, TaskID, StageKey string
	PrincipalID, Harness, Provider, Model                                  string
	ConnectionID                                                           string
	// AuthMethod is the leased connection's: api_key, or a personal
	// subscription (claude_setup_token, codex_chatgpt) served only as the
	// owner's own single route. AccountID is a Codex sign-in's ChatGPT
	// account; OwnerID is a personal connection's owner.
	AuthMethod, AccountID, OwnerID string
	ExpiresAt                      time.Time
	Key                            []byte
}

// Authorizer checks a token on every request (§3 step 3): the hash lookup,
// then the live lease and attempt, the organization switch, and the grant,
// policy and connection through access.AuthorizeModelInvoke, the same code
// the bootstrap release path uses. Revoking a lease or grant, stopping the
// attempt, or turning the gateway off denies the very next request.
//
// ponytail: no token cache. §3 allows at most 5 s; one indexed query per
// request is cheap and makes revocation immediate. Add a cache when the
// gateway needs more than the database can serve.
type Authorizer struct {
	DB      *pgxpool.Pool
	Secrets *access.SecretStore
	// OAuth supplies a fresh access token for an owner's Codex sign-in when
	// the organization serves personal subscription routes (§6). Nil keeps
	// Codex sign-ins off the gateway.
	OAuth *access.OAuthRefresher
}

func (a *Authorizer) Authorize(ctx context.Context, token, family string) (Grant, error) {
	if a == nil || a.DB == nil || a.Secrets == nil || !validToken(token) {
		return Grant{}, ErrDenied
	}
	hash := HashToken(token)
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return Grant{}, err
	}
	defer tx.Rollback(ctx)
	var g Grant
	var invoke access.ModelInvoke
	var families []string
	var leaseConnection string
	var enabled, personalRoutes bool
	err = tx.QueryRow(ctx, `SELECT t.id,t.organization_id,t.project_id,t.run_id,t.attempt_id,t.task_id,t.stage_key,
		t.principal_id,t.harness,t.provider,t.model,t.allowed_families,t.binding_id,t.grantee_kind,t.grantee_id,
		t.policy_version,l.connection_id,l.expires_at,COALESCE(s.enabled,false),COALESCE(s.personal_routes_enabled,false)
		FROM gateway_tokens t
		JOIN access_leases l ON l.organization_id=t.organization_id::text AND l.id=t.lease_id
		JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.id=t.attempt_id
		LEFT JOIN gateway_org_settings s ON s.organization_id=t.organization_id
		WHERE t.token_sha256=$1 AND t.revoked_at IS NULL
		AND l.capability='model.invoke' AND l.revoked_at IS NULL AND l.delivered_at IS NOT NULL
		AND l.expires_at>clock_timestamp() AND l.owner_generation=t.lease_generation
		AND l.attempt_id=t.attempt_id::text AND l.binding_id=t.binding_id
		AND a.state='running' AND a.generation=t.lease_generation
		FOR SHARE OF t,l,a`, hash[:]).Scan(&g.TokenID, &g.OrganizationID, &g.ProjectID, &g.RunID, &g.AttemptID,
		&g.TaskID, &g.StageKey, &g.PrincipalID, &g.Harness, &g.Provider, &g.Model, &families,
		&invoke.BindingID, &invoke.GranteeKind, &invoke.GranteeID, &invoke.PolicyVersion,
		&leaseConnection, &g.ExpiresAt, &enabled, &personalRoutes)
	if errors.Is(err, pgx.ErrNoRows) {
		return Grant{}, ErrDenied
	}
	if err != nil {
		return Grant{}, fmt.Errorf("gateway token lookup: %w", err)
	}
	if !enabled {
		return Grant{}, ErrDisabled
	}
	if !slices.Contains(families, family) || g.Provider != family {
		return Grant{}, ErrDenied
	}
	invoke.OrganizationID, invoke.ProjectID, invoke.AttemptID = g.OrganizationID, g.ProjectID, g.AttemptID
	invoke.Provider, invoke.Model = g.Provider, g.Model
	// AuthorizeModelInvoke applies the owner-only rule to personal
	// subscriptions: the grantee is the owning user, never a workload, team
	// or another member, and the run initiator is that user.
	decision, err := access.AuthorizeModelInvoke(ctx, tx, invoke)
	if err != nil {
		if errors.Is(err, access.ErrDenied) {
			return Grant{}, ErrDenied
		}
		return Grant{}, err
	}
	if decision.ConnectionID != leaseConnection {
		return Grant{}, ErrDenied
	}
	var ownerKind string
	if err := tx.QueryRow(ctx, `SELECT auth_method,owner_kind,owner_id FROM access_connections
		WHERE organization_id=$1 AND id=$2`, g.OrganizationID, decision.ConnectionID).Scan(&g.AuthMethod, &ownerKind, &g.OwnerID); err != nil {
		return Grant{}, deniedOr(err)
	}
	personal := subscriptionAuth(g.AuthMethod)
	if personal && (ownerKind != "user" || invoke.GranteeKind != "user" || invoke.GranteeID != g.OwnerID ||
		g.PrincipalID != g.OwnerID) {
		return Grant{}, ErrDenied // belt and braces: only the owner's own run.
	}
	if !personal {
		g.OwnerID = ""
	}
	g.ConnectionID = decision.ConnectionID
	switch {
	case g.AuthMethod == access.CodexSubscriptionAuth:
		// §6: the refresh token stays in central custody; the gateway sends
		// a fresh access token for the owner's own run and nothing reaches
		// the sandbox. Only while the org serves personal subscription routes.
		if decision.DeliveryMode != "oauth_access" || !personalRoutes || a.OAuth == nil {
			return Grant{}, ErrDenied
		}
		if err := tx.Commit(ctx); err != nil {
			return Grant{}, err
		}
		delivery, err := a.OAuth.Deliver(ctx, g.OrganizationID, g.ConnectionID, time.Now().Add(2*time.Minute))
		if err != nil {
			if errors.Is(err, access.ErrDenied) || errors.Is(err, access.ErrReconnect) {
				return Grant{}, ErrDenied
			}
			return Grant{}, err
		}
		clear(delivery.File)
		g.Key, g.AccountID = delivery.AccessToken, delivery.AccountID
		return g, nil
	case decision.DeliveryMode != ModeNative && decision.DeliveryMode != ModeBrokered:
		return Grant{}, ErrDenied
	}
	secret, err := a.Secrets.ReadCurrent(ctx, tx, g.OrganizationID, decision.ConnectionID)
	if err != nil {
		return Grant{}, deniedOr(err)
	}
	if secret.ExpiresAt != nil && !secret.ExpiresAt.After(time.Now()) {
		secret.Clear()
		return Grant{}, ErrDenied
	}
	if err := tx.Commit(ctx); err != nil {
		secret.Clear()
		return Grant{}, err
	}
	g.Key = secret.Bytes
	return g, nil
}

func deniedOr(err error) error {
	if errors.Is(err, access.ErrDenied) || errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

// RouteKey reads a pooled route's credential for a request the grant
// already authorized. The route must still be an active, organization-owned
// API-key or cloud connection in the request's organization: never a
// personal subscription. The caller clears the bytes.
func (a *Authorizer) RouteKey(ctx context.Context, g Grant, route Route) ([]byte, error) {
	if a == nil || a.DB == nil || a.Secrets == nil || subscriptionAuth(route.AuthMethod) || route.Personal() {
		return nil, ErrDenied
	}
	tx, err := a.DB.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var method string
	if err := tx.QueryRow(ctx, `SELECT auth_method FROM access_connections WHERE organization_id=$1 AND id=$2
		AND state='active' AND owner_kind='organization' FOR SHARE`, g.OrganizationID, route.ConnectionID).Scan(&method); err != nil {
		return nil, deniedOr(err)
	}
	if subscriptionAuth(method) || method != route.AuthMethod {
		return nil, ErrDenied
	}
	secret, err := a.Secrets.ReadCurrent(ctx, tx, g.OrganizationID, route.ConnectionID)
	if err != nil {
		return nil, deniedOr(err)
	}
	if secret.ExpiresAt != nil && !secret.ExpiresAt.After(time.Now()) {
		secret.Clear()
		return nil, ErrDenied
	}
	return secret.Bytes, tx.Commit(ctx)
}
