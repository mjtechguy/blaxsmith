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
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Grant is one authorized request: the token's bound context plus the route
// credential. Key must be cleared by the caller once the request is sent.
type Grant struct {
	TokenID, OrganizationID, ProjectID, RunID, AttemptID, TaskID, StageKey string
	PrincipalID, Harness, Provider, Model                                  string
	ConnectionID                                                           string
	ExpiresAt                                                              time.Time
	Key                                                                    []byte
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
}

func (a *Authorizer) Authorize(ctx context.Context, token, family string) (Grant, error) {
	ctx = tenant.System(ctx) // the token hash is the only credential; its row names the organization
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
	var enabled bool
	err = tx.QueryRow(ctx, `SELECT t.id,t.organization_id,t.project_id,t.run_id,t.attempt_id,t.task_id,t.stage_key,
		t.principal_id,t.harness,t.provider,t.model,t.allowed_families,t.binding_id,t.grantee_kind,t.grantee_id,
		t.policy_version,l.connection_id,l.expires_at,COALESCE(s.enabled,false)
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
		&leaseConnection, &g.ExpiresAt, &enabled)
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
	decision, err := access.AuthorizeModelInvoke(ctx, tx, invoke)
	if err != nil {
		if errors.Is(err, access.ErrDenied) {
			return Grant{}, ErrDenied
		}
		return Grant{}, err
	}
	if (decision.DeliveryMode != ModeNative && decision.DeliveryMode != ModeBrokered) || decision.ConnectionID != leaseConnection {
		return Grant{}, ErrDenied
	}
	secret, err := a.Secrets.ReadCurrent(ctx, tx, g.OrganizationID, decision.ConnectionID)
	if err != nil {
		if errors.Is(err, access.ErrDenied) {
			return Grant{}, ErrDenied
		}
		return Grant{}, err
	}
	if secret.ExpiresAt != nil && !secret.ExpiresAt.After(time.Now()) {
		secret.Clear()
		return Grant{}, ErrDenied
	}
	if err := tx.Commit(ctx); err != nil {
		secret.Clear()
		return Grant{}, err
	}
	g.ConnectionID, g.Key = decision.ConnectionID, secret.Bytes
	return g, nil
}
