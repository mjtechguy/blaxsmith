package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const accessLifetime = 10 * time.Minute
const sessionLifetime = 7 * 24 * time.Hour

var ErrUnauthenticated = errors.New("authentication failed")
var ErrRefreshReuse = errors.New("refresh token reused; session revoked")
var ErrSessionConfiguration = errors.New("invalid session signing configuration")

type SessionManager struct {
	db     *pgxpool.Pool
	issuer string
	signer ed25519.PrivateKey
	keyID  string
	keys   map[string]ed25519.PublicKey
}

type Tokens struct {
	Access        string
	Refresh       string
	AccessExpires time.Time
	SessionID     string
	Organization  string
	Principal     string
	Role          string
}

type Caller struct {
	OrganizationID string
	PrincipalID    string
	SessionID      string
	Role           string
}

type accessClaims struct {
	OrganizationID string `json:"org_id"`
	SessionID      string `json:"sid"`
	Role           string `json:"role"`
	jwt.RegisteredClaims
}

// Previous public keys may overlap only while their access tokens remain live.
// Refresh tokens are server-side and always mint with the current signer.
func NewSessionManager(db *pgxpool.Pool, issuer string, signer ed25519.PrivateKey, previous ...ed25519.PublicKey) (*SessionManager, error) {
	if db == nil || issuer == "" || len(signer) != ed25519.PrivateKeySize {
		return nil, ErrSessionConfiguration
	}
	copyKey := append(ed25519.PrivateKey(nil), signer...)
	seed := copyKey.Seed()
	valid := subtle.ConstantTimeCompare(copyKey, ed25519.NewKeyFromSeed(seed)) == 1
	clear(seed)
	if !valid {
		clear(copyKey)
		return nil, ErrSessionConfiguration
	}
	active := append(ed25519.PublicKey(nil), copyKey.Public().(ed25519.PublicKey)...)
	keys := map[string]ed25519.PublicKey{signingKeyID(active): active}
	for _, key := range previous {
		if len(key) != ed25519.PublicKeySize {
			clear(copyKey)
			return nil, ErrSessionConfiguration
		}
		keys[signingKeyID(key)] = append(ed25519.PublicKey(nil), key...)
	}
	return &SessionManager{db: db, issuer: issuer, signer: copyKey, keyID: signingKeyID(active), keys: keys}, nil
}

func signingKeyID(public ed25519.PublicKey) string {
	digest := sha256.Sum256(public)
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// LoadSessionSigner reads a 32-byte seed from a platform-mounted file. It
// permits group read for Kubernetes Secret mounts but never world access.
func LoadSessionSigner(path string) (ed25519.PrivateKey, error) {
	file, err := os.Open(path) // #nosec G304 -- path is trusted platform configuration; opened file is mode-checked
	if err != nil {
		return nil, fmt.Errorf("open session signer: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o027 != 0 || info.Size() != ed25519.SeedSize {
		return nil, ErrSessionConfiguration
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(file, seed); err != nil {
		clear(seed)
		return nil, ErrSessionConfiguration
	}
	signer := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return signer, nil
}

// LoginLocal is an internal operation. Do not expose it over HTTP until login
// throttling, secure cookies, CSRF/origin checks, and response redaction exist.
func (m *SessionManager) LoginLocal(ctx context.Context, slug, username string, password []byte) (Tokens, error) {
	if m == nil || !organizationSlug.MatchString(slug) || !accountName.MatchString(username) || len(password) > passwordMax {
		return Tokens{}, ErrUnauthenticated
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback(ctx)
	var orgID, principalID, principalState, membershipState, role, loginPolicy, mfaPolicy string
	var encoded *string
	err = tx.QueryRow(ctx, `SELECT o.id,p.id,p.password_hash,p.state,m.state,m.role,o.login_policy,o.mfa_policy
		FROM identity_organizations o
		JOIN identity_memberships m ON m.organization_id=o.id
		JOIN identity_principals p ON p.id=m.principal_id
		WHERE o.slug=$1 AND p.username=$2 FOR SHARE OF o,m,p`, slug, username).
		Scan(&orgID, &principalID, &encoded, &principalState, &membershipState, &role, &loginPolicy, &mfaPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		burnPasswordAttempt(password)
		return Tokens{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("read local identity: %w", err)
	}
	validPassword := false
	if encoded != nil {
		validPassword = VerifyPassword(*encoded, password)
	} else {
		burnPasswordAttempt(password)
	}
	if !validPassword || principalState != "active" || membershipState != "active" ||
		(loginPolicy != "local" && loginPolicy != "mixed") || mfaPolicy != "optional" {
		return Tokens{}, ErrUnauthenticated
	}
	var sessionID string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid()`).Scan(&sessionID); err != nil {
		return Tokens{}, fmt.Errorf("allocate session: %w", err)
	}
	refresh, hash, err := newRefresh()
	if err != nil {
		return Tokens{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO identity_sessions
		(organization_id,id,principal_id,auth_method,mfa_level,expires_at)
		VALUES ($1,$2,$3,'local','none',$4)`, orgID, sessionID, principalID, now.Add(sessionLifetime)); err != nil {
		return Tokens{}, fmt.Errorf("create session: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_refresh_tokens (token_hash,organization_id,session_id)
		VALUES ($1,$2,$3)`, hash[:], orgID, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("create refresh token: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'identity.login',$3)`, orgID, principalID, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("audit login: %w", err)
	}
	access, expires, err := m.sign(now, orgID, principalID, sessionID, role)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, fmt.Errorf("commit session: %w", err)
	}
	return Tokens{access, refresh, expires, sessionID, orgID, principalID, role}, nil
}

func (m *SessionManager) ValidateAccess(ctx context.Context, raw string) (Caller, error) {
	if m == nil || len(raw) == 0 || len(raw) > 4096 {
		return Caller{}, ErrUnauthenticated
	}
	claims := &accessClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, ErrUnauthenticated
		}
		key, ok := m.keys[kid]
		if !ok {
			return nil, ErrUnauthenticated
		}
		return key, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodEdDSA.Alg()}), jwt.WithIssuer(m.issuer),
		jwt.WithAudience("blaxsmith-api"), jwt.WithExpirationRequired(), jwt.WithNotBeforeRequired(), jwt.WithIssuedAt())
	if err != nil || !token.Valid || claims.Subject == "" || claims.OrganizationID == "" || claims.SessionID == "" ||
		claims.IssuedAt == nil || claims.ExpiresAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > accessLifetime {
		return Caller{}, ErrUnauthenticated
	}
	var role string
	err = m.db.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')`,
		claims.OrganizationID, claims.SessionID, claims.Subject).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != claims.Role) {
		return Caller{}, ErrUnauthenticated
	}
	if err != nil {
		return Caller{}, fmt.Errorf("validate session: %w", err)
	}
	return Caller{claims.OrganizationID, claims.Subject, claims.SessionID, role}, nil
}

func (m *SessionManager) Refresh(ctx context.Context, raw string) (Tokens, error) {
	if m == nil || len(raw) != 43 {
		return Tokens{}, ErrUnauthenticated
	}
	secret, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(secret) != 32 {
		return Tokens{}, ErrUnauthenticated
	}
	hash := sha256.Sum256(secret)
	clear(secret)
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback(ctx)
	var orgID, sessionID string
	err = tx.QueryRow(ctx, `SELECT organization_id,session_id FROM identity_refresh_tokens
		WHERE token_hash=$1`, hash[:]).Scan(&orgID, &sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("find refresh token: %w", err)
	}
	var principalID, authMethod, mfaLevel string
	var revoked *time.Time
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT principal_id,auth_method,mfa_level,expires_at,revoked_at
		FROM identity_sessions WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, sessionID).
		Scan(&principalID, &authMethod, &mfaLevel, &expires, &revoked)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("lock session: %w", err)
	}
	var consumed *time.Time
	if err := tx.QueryRow(ctx, `SELECT consumed_at FROM identity_refresh_tokens
		WHERE token_hash=$1 FOR UPDATE`, hash[:]).Scan(&consumed); err != nil {
		return Tokens{}, fmt.Errorf("lock refresh token: %w", err)
	}
	if consumed != nil {
		if _, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
			WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL`, orgID, sessionID); err != nil {
			return Tokens{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
			(organization_id,actor_kind,actor_id,action,subject_id)
			VALUES ($1,'system',NULL,'identity.refresh_reuse',$2)`, orgID, sessionID); err != nil {
			return Tokens{}, fmt.Errorf("audit refresh reuse: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return Tokens{}, err
		}
		return Tokens{}, ErrRefreshReuse
	}
	if revoked != nil || !expires.After(time.Now()) {
		return Tokens{}, ErrUnauthenticated
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT m.role FROM identity_memberships m
		JOIN identity_principals p ON p.id=m.principal_id
		JOIN identity_organizations o ON o.id=m.organization_id
		WHERE m.organization_id=$1 AND m.principal_id=$2
		AND m.state='active' AND p.state='active'
		AND ($3<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR $4='totp')`,
		orgID, principalID, authMethod, mfaLevel).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("read current membership: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_refresh_tokens SET consumed_at=clock_timestamp()
		WHERE token_hash=$1 AND consumed_at IS NULL`, hash[:]); err != nil {
		return Tokens{}, fmt.Errorf("consume refresh token: %w", err)
	}
	next, nextHash, err := newRefresh()
	if err != nil {
		return Tokens{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_refresh_tokens (token_hash,organization_id,session_id)
		VALUES ($1,$2,$3)`, nextHash[:], orgID, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("rotate refresh token: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'identity.refresh',$3)`, orgID, principalID, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("audit refresh: %w", err)
	}
	now := time.Now().UTC()
	access, accessExpires, err := m.sign(now, orgID, principalID, sessionID, role)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, fmt.Errorf("commit refresh: %w", err)
	}
	return Tokens{access, next, accessExpires, sessionID, orgID, principalID, role}, nil
}

func (m *SessionManager) Revoke(ctx context.Context, caller Caller) error {
	if m == nil || caller.OrganizationID == "" || caller.PrincipalID == "" || caller.SessionID == "" {
		return ErrUnauthenticated
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	result, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND principal_id=$3 AND revoked_at IS NULL`,
		caller.OrganizationID, caller.SessionID, caller.PrincipalID)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrUnauthenticated
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'identity.logout',$3)`,
		caller.OrganizationID, caller.PrincipalID, caller.SessionID); err != nil {
		return fmt.Errorf("audit logout: %w", err)
	}
	return tx.Commit(ctx)
}

func (m *SessionManager) sign(now time.Time, orgID, principalID, sessionID, role string) (string, time.Time, error) {
	expires := now.Add(accessLifetime)
	claims := accessClaims{OrganizationID: orgID, SessionID: sessionID, Role: role,
		RegisteredClaims: jwt.RegisteredClaims{Issuer: m.issuer, Subject: principalID,
			Audience: jwt.ClaimStrings{"blaxsmith-api"}, IssuedAt: jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(expires)}}
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	token.Header["kid"] = m.keyID
	encoded, err := token.SignedString(m.signer)
	return encoded, expires, err
}

func newRefresh() (string, [32]byte, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return "", [32]byte{}, err
	}
	text := base64.RawURLEncoding.EncodeToString(secret[:])
	hash := sha256.Sum256(secret[:])
	clear(secret[:])
	return text, hash, nil
}
