package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/netip"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrOAuthGrant = errors.New("invalid OAuth grant")
var pkceVerifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)
var pkceChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func (m *SessionManager) AllowOAuthExchange(ctx context.Context, source netip.Addr) error {
	if !source.IsValid() {
		return ErrUnauthenticated
	}
	return m.limits.spend(ctx, "oauth_source", source.Unmap().String(), 60)
}

type OAuthGrant struct {
	ClientID, ClientName, RedirectURI, Resource, Challenge, ProjectID string
	Scopes                                                            []string
}

// AuthorizeOAuth records browser consent. Only the opaque code's hash is stored.
// The HTTP boundary additionally enforces the registered client's redirect list.
func (m *SessionManager) AuthorizeOAuth(ctx context.Context, caller Caller, in OAuthGrant) (string, error) {
	if !validUUID(in.ProjectID) || len(in.ClientID) < 1 || len(in.ClientID) > 128 || len(in.ClientName) < 1 || len(in.ClientName) > 80 || len(in.RedirectURI) < 1 || len(in.RedirectURI) > 2048 || in.Resource != m.issuer+"/mcp" || !pkceChallenge.MatchString(in.Challenge) || len(in.Scopes) == 0 || len(in.Scopes) > len(APIScopes) {
		return "", ErrOAuthGrant
	}
	scopes := slices.Clone(in.Scopes)
	slices.Sort(scopes)
	for i, scope := range scopes {
		if !slices.Contains(APIScopes, scope) || i > 0 && scopes[i-1] == scope || caller.Role == "viewer" && scope != "project.read" {
			return "", ErrOAuthGrant
		}
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockBrowserIssuer(ctx, tx, caller); err != nil {
		return "", err
	}
	var project string
	if err := tx.QueryRow(ctx, `SELECT id FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, in.ProjectID).Scan(&project); err != nil {
		return "", ErrOAuthGrant
	}
	// Codes outlive access tokens for reuse detection, then are pruned on consent.
	if _, err := tx.Exec(ctx, `DELETE FROM identity_oauth_codes WHERE organization_id=$1 AND principal_id=$2 AND expires_at<clock_timestamp()-interval '1 day'`, caller.OrganizationID, caller.PrincipalID); err != nil {
		return "", err
	}
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_oauth_codes WHERE organization_id=$1 AND principal_id=$2 AND project_id=$3 AND issued_session_id IS NULL AND expires_at>clock_timestamp()`, caller.OrganizationID, caller.PrincipalID, project).Scan(&active); err != nil {
		return "", err
	}
	if active >= 20 {
		return "", ErrOAuthGrant
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	code := "bxc_" + base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(code))
	encoded, _ := json.Marshal(scopes)
	if _, err := tx.Exec(ctx, `INSERT INTO identity_oauth_codes(organization_id,code_hash,principal_id,browser_session_id,project_id,role,access_expires_at,client_id,client_name,redirect_uri,resource,challenge,scopes) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, caller.OrganizationID, hash[:], caller.PrincipalID, caller.SessionID, project, caller.Role, caller.AccessExpires, in.ClientID, in.ClientName, in.RedirectURI, in.Resource, in.Challenge, string(encoded)); err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,'identity.oauth.authorized',$3)`, caller.OrganizationID, caller.PrincipalID, project); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return code, nil
}

// ExchangeOAuth consumes consent and issues a resource-bound credential in one
// transaction. Redeeming an already used code revokes its previous credential.
func (m *SessionManager) ExchangeOAuth(ctx context.Context, code, clientID, redirect, resource, verifier string) (APIToken, string, error) {
	if len(code) != 47 || !pkceVerifier.MatchString(verifier) || resource != m.issuer+"/mcp" {
		return APIToken{}, "", ErrOAuthGrant
	}
	hash := sha256.Sum256([]byte(code))
	ctx = tenant.System(ctx)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return APIToken{}, "", err
	}
	defer tx.Rollback(ctx)
	var caller Caller
	var in OAuthGrant
	var unexpired bool
	var scopes []byte
	var issued *string
	err = tx.QueryRow(ctx, `SELECT organization_id,principal_id,browser_session_id,role,access_expires_at,client_id,client_name,redirect_uri,resource,challenge,project_id,scopes,expires_at>clock_timestamp(),issued_session_id FROM identity_oauth_codes WHERE code_hash=$1 FOR UPDATE`, hash[:]).Scan(&caller.OrganizationID, &caller.PrincipalID, &caller.SessionID, &caller.Role, &caller.AccessExpires, &in.ClientID, &in.ClientName, &in.RedirectURI, &in.Resource, &in.Challenge, &in.ProjectID, &scopes, &unexpired, &issued)
	if errors.Is(err, pgx.ErrNoRows) {
		return APIToken{}, "", ErrOAuthGrant
	}
	if err != nil {
		return APIToken{}, "", err
	}
	proof := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(proof[:])
	if in.ClientID != clientID || in.Resource != resource || redirect != "" && in.RedirectURI != redirect || subtle.ConstantTimeCompare([]byte(challenge), []byte(in.Challenge)) != 1 {
		return APIToken{}, "", ErrOAuthGrant
	}
	if issued != nil {
		if _, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=COALESCE(revoked_at,clock_timestamp()) WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, *issued); err != nil {
			return APIToken{}, "", err
		}
		if err := tx.Commit(ctx); err != nil {
			return APIToken{}, "", err
		}
		return APIToken{}, "", ErrOAuthGrant
	}
	if !unexpired || json.Unmarshal(scopes, &in.Scopes) != nil {
		return APIToken{}, "", ErrOAuthGrant
	}
	token, raw, err := m.issueAPIToken(ctx, tx, caller, in.ProjectID, "MCP: "+in.ClientName, in.Scopes, time.Hour, "", resource)
	if err != nil {
		return APIToken{}, "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_oauth_codes SET issued_session_id=$2 WHERE code_hash=$1`, hash[:], token.ID); err != nil {
		return APIToken{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return APIToken{}, "", err
	}
	return token, raw, nil
}
