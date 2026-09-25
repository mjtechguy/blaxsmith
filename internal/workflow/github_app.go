package workflow

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var githubClientID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

// GitHubApp is the organization's registered GitHub OAuth App for "Connect
// GitHub". The client secret is an encrypted connection secret, write-only.
type GitHubApp struct {
	ConnectionID, ClientID string
	Configured             bool
}

func (s *Store) GetGitHubApp(ctx context.Context, orgID string) (GitHubApp, error) {
	ctx = tenant.Org(ctx, orgID)
	var app GitHubApp
	err := s.pool.QueryRow(ctx, `SELECT c.id,c.external_account_id,c.active_secret_version IS NOT NULL
		FROM access_connections c JOIN access_provider_registrations p
			ON p.organization_id=c.organization_id AND p.id=c.provider_registration_id
		WHERE c.organization_id=$1 AND c.state='active' AND p.provider_kind=$2
		ORDER BY c.created_at DESC LIMIT 1`, orgID, access.GitHubOAuthProvider).Scan(&app.ConnectionID, &app.ClientID, &app.Configured)
	if errors.Is(err, pgx.ErrNoRows) {
		return GitHubApp{}, nil
	}
	return app, err
}

// SetGitHubAppAs registers or updates the app (org owner/admin). An empty
// secret keeps the stored one; a new client id without a secret is refused.
func (s *Store) SetGitHubAppAs(ctx context.Context, caller identity.Caller, clientID string, secret []byte,
	secrets *access.SecretStore) (GitHubApp, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if !githubClientID.MatchString(clientID) || len(secret) > 512 || strings.ContainsAny(string(secret), "\r\n\x00 ") || secrets == nil {
		return GitHubApp{}, ErrInvalid
	}
	tx, err := s.beginScoped(ctx, caller, ScopeOrganization, "")
	if err != nil {
		return GitHubApp{}, err
	}
	defer tx.Rollback(ctx)
	current, err := s.GetGitHubApp(ctx, caller.OrganizationID)
	if err != nil {
		return GitHubApp{}, err
	}
	if current.ConnectionID != "" && (current.ClientID != clientID || len(secret) > 0) {
		if _, err := tx.Exec(ctx, `UPDATE access_connections SET state='revoked' WHERE organization_id=$1 AND id=$2`,
			caller.OrganizationID, current.ConnectionID); err != nil {
			return GitHubApp{}, err
		}
	} else if current.ConnectionID != "" {
		return current, tx.Commit(ctx)
	}
	if len(secret) == 0 {
		return GitHubApp{}, ErrInvalid
	}
	var providerID, connectionID string
	if err := tx.QueryRow(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,gen_random_uuid()::text,$2,'https://github.com',ARRAY['brokered'],'active') RETURNING id`,
		caller.OrganizationID, access.GitHubOAuthProvider).Scan(&providerID); err != nil {
		return GitHubApp{}, err
	}
	if err := tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,gen_random_uuid()::text,'organization',$1,$2,$3,$4,'active') RETURNING id`,
		caller.OrganizationID, providerID, clientID, access.GitHubOAuthClient).Scan(&connectionID); err != nil {
		return GitHubApp{}, err
	}
	if _, err := secrets.RotateTx(ctx, tx, caller.OrganizationID, connectionID, 0, secret, nil); err != nil {
		return GitHubApp{}, err
	}
	if err := commitAudited(ctx, tx, caller, "access.github_app.configured", connectionID); err != nil {
		return GitHubApp{}, err
	}
	return GitHubApp{ConnectionID: connectionID, ClientID: clientID, Configured: true}, nil
}

// GitHubAppSecret reads the client secret for the platform's own token
// exchange. The caller must Clear it.
func (s *Store) GitHubAppSecret(ctx context.Context, orgID string, secrets *access.SecretStore) (string, access.Secret, error) {
	ctx = tenant.Org(ctx, orgID)
	app, err := s.GetGitHubApp(ctx, orgID)
	if err != nil || !app.Configured || secrets == nil {
		return "", access.Secret{}, errors.Join(ErrNotFound, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", access.Secret{}, err
	}
	defer tx.Rollback(ctx)
	secret, err := secrets.ReadCurrent(ctx, tx, orgID, app.ConnectionID)
	if err != nil {
		return "", access.Secret{}, err
	}
	return app.ClientID, secret, tx.Commit(ctx)
}
