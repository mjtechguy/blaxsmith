package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var APIScopes = []string{"project.read", "goal.write", "run.launch", "run.control", "review.decide"}

type APIToken struct {
	ID, ProjectID, Label, PrincipalID, Kind, Resource string
	Scopes                                            []string
	ExpiresAt                                         time.Time
	RevokedAt                                         *time.Time
}

// IssueAPIToken delegates one project to a separate revocable session. No
// refresh token or browser access token is issued for that session.
func (m *SessionManager) IssueAPIToken(ctx context.Context, caller Caller, project, label string, scopes []string, lifetime time.Duration, service string) (APIToken, string, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return APIToken{}, "", err
	}
	defer tx.Rollback(ctx)
	token, raw, err := m.issueAPIToken(ctx, tx, caller, project, label, scopes, lifetime, service, "")
	if err != nil {
		return APIToken{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return APIToken{}, "", err
	}
	return token, raw, nil
}

func (m *SessionManager) issueAPIToken(ctx context.Context, tx pgx.Tx, caller Caller, project, label string, scopes []string, lifetime time.Duration, service, resource string) (APIToken, string, error) {
	var token APIToken
	if !validUUID(project) || strings.TrimSpace(label) == "" || len(label) > 100 || strings.ContainsRune(label, 0) || lifetime < time.Hour || lifetime > 30*24*time.Hour || len(scopes) == 0 || len(scopes) > len(APIScopes) {
		return token, "", errors.New("invalid project, label, scopes or lifetime (1 hour to 30 days)")
	}
	scopes = slices.Clone(scopes)
	slices.Sort(scopes)
	for i, scope := range scopes {
		if !slices.Contains(APIScopes, scope) || i > 0 && scopes[i-1] == scope || caller.Role == "viewer" && scope != "project.read" {
			return token, "", ErrUnauthenticated
		}
	}
	method, mfa, err := lockBrowserIssuer(ctx, tx, caller)
	if err != nil {
		return token, "", err
	}
	principal, role, kind := caller.PrincipalID, caller.Role, "api"
	if service != "" {
		if !validUUID(service) || (caller.Role != "owner" && caller.Role != "admin") || slices.Contains(scopes, "review.decide") {
			return token, "", ErrUnauthenticated
		}
		var serviceRole string
		if err = tx.QueryRow(ctx, `SELECT m.role FROM identity_service_principals sp JOIN identity_memberships m ON m.organization_id=sp.organization_id AND m.principal_id=sp.principal_id JOIN identity_principals p ON p.id=sp.principal_id WHERE sp.organization_id=$1 AND sp.project_id=$2 AND sp.principal_id=$3 AND m.state='active' AND m.role='member' AND p.state='active' AND p.kind='service' FOR SHARE OF m,p`, caller.OrganizationID, project, service).Scan(&serviceRole); err != nil {
			return token, "", ErrUnauthenticated
		}
		principal, role, kind, method, mfa = service, serviceRole, "service", "service", "none"
	}
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT true FROM workflow_projects WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, project).Scan(&exists); err != nil {
		return token, "", err
	}
	if !exists {
		return token, "", ErrUnauthenticated
	}
	var active int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM identity_api_tokens t JOIN identity_sessions s ON s.organization_id=t.organization_id AND s.id=t.session_id WHERE t.organization_id=$1 AND t.project_id=$2 AND t.principal_id=$3 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()`, caller.OrganizationID, project, principal).Scan(&active); err != nil {
		return token, "", err
	}
	if active >= 100 {
		return token, "", errors.New("revoke an existing credential before creating more than 100 active project credentials")
	}
	var random [32]byte
	if _, err = rand.Read(random[:]); err != nil {
		return token, "", err
	}
	raw := "bxs_" + base64.RawURLEncoding.EncodeToString(random[:])
	hash := sha256.Sum256([]byte(raw))
	clear(random[:])
	token = APIToken{ProjectID: project, Label: label, Scopes: scopes, PrincipalID: principal, Kind: kind, Resource: resource}
	if err = tx.QueryRow(ctx, `INSERT INTO identity_sessions(organization_id,id,principal_id,auth_method,mfa_level,expires_at,credential_kind,user_agent)
 VALUES($1,gen_random_uuid(),$2,$3,$4,clock_timestamp()+$5*interval '1 second',$6,'Blaxsmith machine API') RETURNING id,expires_at`, caller.OrganizationID, principal, method, mfa, int64(lifetime/time.Second), kind).Scan(&token.ID, &token.ExpiresAt); err != nil {
		return APIToken{}, "", err
	}
	encoded, _ := json.Marshal(scopes)
	if _, err = tx.Exec(ctx, `INSERT INTO identity_api_tokens(organization_id,session_id,project_id,principal_id,token_hash,label,scopes,role,resource) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, caller.OrganizationID, token.ID, project, principal, hash[:], label, string(encoded), role, resource); err != nil {
		return APIToken{}, "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,'identity.api_token.created',$3)`, caller.OrganizationID, caller.PrincipalID, token.ID); err != nil {
		return APIToken{}, "", err
	}
	return token, raw, nil
}

func (m *SessionManager) AuthenticateAPI(ctx context.Context, raw string) (Caller, APIToken, error) {
	var caller Caller
	var token APIToken
	var scopes []byte
	if len(raw) != 47 || !strings.HasPrefix(raw, "bxs_") {
		return caller, token, ErrUnauthenticated
	}
	hash := sha256.Sum256([]byte(raw))
	err := m.db.QueryRow(tenant.System(ctx), `SELECT t.organization_id,t.principal_id,t.session_id,t.role,t.project_id,t.label,t.scopes,s.expires_at,s.credential_kind,t.resource
 FROM identity_api_tokens t JOIN identity_sessions s ON s.organization_id=t.organization_id AND s.id=t.session_id
 WHERE t.token_hash=$1 AND s.credential_kind IN ('api','service') AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()`, hash[:]).Scan(&caller.OrganizationID, &caller.PrincipalID, &caller.SessionID, &caller.Role, &token.ProjectID, &token.Label, &scopes, &caller.AccessExpires, &token.Kind, &token.Resource)
	if errors.Is(err, pgx.ErrNoRows) {
		return Caller{}, APIToken{}, ErrUnauthenticated
	}
	if err != nil {
		return Caller{}, APIToken{}, err
	}
	if err = json.Unmarshal(scopes, &token.Scopes); err != nil {
		return Caller{}, APIToken{}, err
	}
	caller, err = m.liveSession(ctx, caller)
	if err != nil {
		return Caller{}, APIToken{}, err
	}
	token.ID, token.ExpiresAt, token.PrincipalID = caller.SessionID, caller.AccessExpires, caller.PrincipalID
	return caller, token, nil
}

func (m *SessionManager) ListAPITokens(ctx context.Context, caller Caller, project, service string) ([]APIToken, error) {
	if !validUUID(project) {
		return nil, ErrUnauthenticated
	}
	principal := caller.PrincipalID
	if service != "" {
		if !validUUID(service) || (caller.Role != "owner" && caller.Role != "admin") {
			return nil, ErrUnauthenticated
		}
		var id string
		if err := m.db.QueryRow(tenant.Org(ctx, caller.OrganizationID), `SELECT principal_id FROM identity_service_principals WHERE organization_id=$1 AND project_id=$2 AND principal_id=$3`, caller.OrganizationID, project, service).Scan(&id); err != nil {
			return nil, ErrUnauthenticated
		}
		principal = service
	}
	rows, err := m.db.Query(tenant.Org(ctx, caller.OrganizationID), `SELECT t.session_id,t.project_id,t.label,t.scopes,s.expires_at,s.revoked_at,t.principal_id,s.credential_kind,t.resource FROM identity_api_tokens t
 JOIN identity_sessions s ON s.organization_id=t.organization_id AND s.id=t.session_id
 WHERE t.organization_id=$1 AND t.principal_id=$2 AND t.project_id=$3 ORDER BY (s.revoked_at IS NULL AND s.expires_at>clock_timestamp()) DESC,s.created_at DESC LIMIT 100`, caller.OrganizationID, principal, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tokens := []APIToken{}
	for rows.Next() {
		var token APIToken
		var scopes []byte
		if err = rows.Scan(&token.ID, &token.ProjectID, &token.Label, &scopes, &token.ExpiresAt, &token.RevokedAt, &token.PrincipalID, &token.Kind, &token.Resource); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(scopes, &token.Scopes); err != nil {
			return nil, err
		}
		tokens = append(tokens, token)
	}
	return tokens, rows.Err()
}

func (m *SessionManager) RevokeAPIToken(ctx context.Context, caller Caller, id string) error {
	if !validUUID(id) {
		return ErrUnauthenticated
	}
	ctx = tenant.Org(ctx, caller.OrganizationID)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Revalidate the browser session; machine credentials cannot manage tokens.
	var live string
	err = tx.QueryRow(ctx, `SELECT s.id FROM identity_sessions s JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
 JOIN identity_principals p ON p.id=s.principal_id JOIN identity_organizations o ON o.id=s.organization_id
 WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3 AND s.credential_kind='browser' AND NOT s.email_required
 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND $4::timestamptz>clock_timestamp()
 AND m.state='active' AND m.role=$5 AND p.state='active'
 AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed')) AND (o.mfa_policy<>'required' OR s.mfa_level='totp') FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID, caller.AccessExpires, caller.Role).Scan(&live)
	if err != nil {
		return ErrUnauthenticated
	}
	result, err := tx.Exec(ctx, `UPDATE identity_sessions s SET revoked_at=COALESCE(s.revoked_at,clock_timestamp()) FROM identity_api_tokens t WHERE t.organization_id=s.organization_id AND t.session_id=s.id AND t.organization_id=$1 AND t.session_id=$3 AND (t.principal_id=$2 OR ($4 IN ('owner','admin') AND EXISTS (SELECT 1 FROM identity_service_principals sp WHERE sp.organization_id=t.organization_id AND sp.principal_id=t.principal_id AND sp.project_id=t.project_id)))`, caller.OrganizationID, caller.PrincipalID, id, caller.Role)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrUnauthenticated
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identity_audit_events(organization_id,actor_kind,actor_id,action,subject_id) VALUES($1,'principal',$2,'identity.api_token.revoked',$3)`, caller.OrganizationID, caller.PrincipalID, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func lockBrowserIssuer(ctx context.Context, tx pgx.Tx, caller Caller) (string, string, error) {
	var method, mfa string
	err := tx.QueryRow(ctx, `SELECT s.auth_method,s.mfa_level FROM identity_sessions s
 JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
 JOIN identity_principals p ON p.id=s.principal_id JOIN identity_organizations o ON o.id=s.organization_id
 WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3 AND m.role=$4
 AND s.credential_kind='browser' AND NOT s.email_required AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
 AND $5::timestamptz>clock_timestamp() AND m.state='active' AND p.state='active'
 AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed')) AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
 FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID, caller.Role, caller.AccessExpires).Scan(&method, &mfa)
	if err != nil {
		return "", "", ErrUnauthenticated
	}
	return method, mfa, nil
}
