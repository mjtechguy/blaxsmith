package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

const accessLifetime = 10 * time.Minute

// refreshGrace is how long a rotated-out refresh token still answers with its
// successor instead of tripping reuse detection: two tabs refreshing at once,
// or a client that lost the response when the server restarted mid-refresh.
const refreshGrace = time.Minute

// AccessLifetime and RefreshGrace are reported to administrators.
const AccessLifetime, RefreshGrace = accessLifetime, refreshGrace

// SessionPolicy bounds a browser session. Each refresh slides expiry to
// Idle from now, but never past Absolute from sign-in.
type SessionPolicy struct {
	Idle     time.Duration
	Absolute time.Duration
}

// DefaultSessionPolicy is 7 days idle, 30 days absolute.
var DefaultSessionPolicy = SessionPolicy{Idle: 7 * 24 * time.Hour, Absolute: 30 * 24 * time.Hour}

// Validate requires an idle timeout that outlives several access tokens and
// an absolute lifetime of at least the idle timeout and at most a year.
func (p SessionPolicy) Validate() error {
	if p.Idle < 15*time.Minute || p.Absolute < p.Idle || p.Absolute > 366*24*time.Hour {
		return fmt.Errorf("%w: session idle timeout must be at least 15m and absolute lifetime between it and 366d",
			ErrSessionConfiguration)
	}
	return nil
}

var ErrUnauthenticated = errors.New("authentication failed")
var ErrRefreshReuse = errors.New("refresh token reused; session revoked")
var ErrSessionConfiguration = errors.New("invalid session signing configuration")

type SessionManager struct {
	db            *pgxpool.Pool
	issuer        string
	signer        ed25519.PrivateKey
	keyID         string
	keys          map[string]ed25519.PublicKey
	limits        *LoginLimit
	passwordSlots chan struct{}
	policy        SessionPolicy
	// successorKey derives each rotated refresh token from its predecessor, so
	// a replay inside refreshGrace can be answered with the same successor
	// without storing any refresh token in plaintext.
	successorKey []byte
}

type Tokens struct {
	Access         string
	Refresh        string
	AccessExpires  time.Time
	SessionExpires time.Time
	SessionID      string
	Organization   string
	Principal      string
	Role           string
	EmailRequired  bool
}

type Caller struct {
	OrganizationID string
	PrincipalID    string
	SessionID      string
	Role           string
	AccessExpires  time.Time
	// EmailRequired marks a one-time legacy-username session: it may only set
	// an email (BrowserGuard.AccountCaller) until one is saved.
	EmailRequired bool
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
	derive := hmac.New(sha256.New, seed)
	derive.Write([]byte("blaxsmith refresh successor v1"))
	successorKey := derive.Sum(nil)
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
	limits, err := NewLoginLimit(db)
	if err != nil {
		clear(copyKey)
		return nil, err
	}
	return &SessionManager{db: db, issuer: issuer, signer: copyKey, keyID: signingKeyID(active), keys: keys,
		limits: limits, passwordSlots: make(chan struct{}, 2), policy: DefaultSessionPolicy, successorKey: successorKey}, nil
}

// SetPolicy replaces the session lifetime policy. Call it before serving.
func (m *SessionManager) SetPolicy(policy SessionPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	m.policy = policy
	return nil
}

// Policy reports the session lifetime policy in force.
func (m *SessionManager) Policy() SessionPolicy { return m.policy }

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

// LoadSessionPublicKey reads a prior signer's raw public key. Public read is
// permitted, but a writable mount cannot be trusted for token verification.
func LoadSessionPublicKey(path string) (ed25519.PublicKey, error) {
	file, err := os.Open(path) // #nosec G304 -- path is trusted platform configuration; opened file is mode-checked
	if err != nil {
		return nil, fmt.Errorf("open previous session public key: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Size() != ed25519.PublicKeySize {
		return nil, ErrSessionConfiguration
	}
	key := make([]byte, ed25519.PublicKeySize)
	if _, err := io.ReadFull(file, key); err != nil {
		return nil, ErrSessionConfiguration
	}
	return ed25519.PublicKey(key), nil
}

// ErrOrganizationRequired is returned only after the password is verified,
// when the account belongs to several organizations and none was named.
var ErrOrganizationRequired = errors.New("choose the organization to sign in to")

// LoginLocal is an internal operation. The source must be the authenticated
// network peer, not an untrusted forwarding header. Browser transport still
// needs secure cookies, CSRF/origin checks, and response redaction.
//
// login is an email. For a principal that has no email yet, its legacy
// username is accepted exactly once; that session is flagged email_required
// and can only set an email. Unknown accounts, used legacy logins, and wrong
// passwords all fail the same way after the same Argon2 work. slug may be
// empty when the account has a single organization.
func (m *SessionManager) LoginLocal(ctx context.Context, slug, login string, password []byte, source netip.Addr, userAgent string) (Tokens, error) {
	ctx = tenant.System(ctx) // no organization is known until the credential is matched
	login = strings.ToLower(strings.TrimSpace(login))
	slug = strings.TrimSpace(slug)
	email, emailErr := NormalizeEmail(login)
	legacy := emailErr != nil
	if m == nil || (slug != "" && !organizationSlug.MatchString(slug)) || (legacy && !accountName.MatchString(login)) ||
		len(password) > passwordMax {
		return Tokens{}, ErrUnauthenticated
	}
	if !legacy {
		login = email
	}
	if err := m.limits.Allow(ctx, login, source); err != nil {
		return Tokens{}, err
	}
	if len(password) < passwordMin || !utf8.Valid(password) {
		return Tokens{}, ErrUnauthenticated
	}
	select {
	case m.passwordSlots <- struct{}{}:
		defer func() { <-m.passwordSlots }()
	case <-ctx.Done():
		return Tokens{}, ctx.Err()
	}
	var principalID string
	var encoded *string
	err := m.db.QueryRow(ctx, `SELECT id,password_hash FROM identity_principals
		WHERE CASE WHEN $2 THEN username=$1 AND email IS NULL AND legacy_login_at IS NULL
			ELSE lower(email)=$1 END`, login, legacy).Scan(&principalID, &encoded)
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
	if !validPassword || encoded == nil {
		return Tokens{}, ErrUnauthenticated
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return Tokens{}, err
	}
	defer tx.Rollback(ctx)
	// A legacy login locks the principal so two concurrent attempts cannot
	// both spend the single use.
	lock := "FOR SHARE OF o,m,p"
	if legacy {
		lock = "FOR SHARE OF o,m FOR UPDATE OF p"
	}
	rows, err := tx.Query(ctx, `SELECT o.id,m.role
		FROM identity_organizations o
		JOIN identity_memberships m ON m.organization_id=o.id
		JOIN identity_principals p ON p.id=m.principal_id
		WHERE p.id=$1 AND p.password_hash=$2 AND ($3='' OR o.slug=$3)
		AND (NOT $4 OR (p.email IS NULL AND p.legacy_login_at IS NULL))
		AND p.state='active' AND m.state='active'
		AND o.login_policy IN ('local','mixed') AND o.mfa_policy='optional'
		ORDER BY o.slug `+lock, principalID, *encoded, slug, legacy)
	if err != nil {
		return Tokens{}, fmt.Errorf("recheck local identity: %w", err)
	}
	type membership struct{ org, role string }
	memberships, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (membership, error) {
		var m membership
		return m, row.Scan(&m.org, &m.role)
	})
	if err != nil {
		return Tokens{}, fmt.Errorf("recheck local identity: %w", err)
	}
	if len(memberships) == 0 {
		return Tokens{}, ErrUnauthenticated
	}
	if len(memberships) > 1 {
		return Tokens{}, ErrOrganizationRequired
	}
	orgID, role := memberships[0].org, memberships[0].role
	if legacy {
		if _, err := tx.Exec(ctx, `UPDATE identity_principals SET legacy_login_at=clock_timestamp() WHERE id=$1`, principalID); err != nil {
			return Tokens{}, fmt.Errorf("record legacy login: %w", err)
		}
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
		(organization_id,id,principal_id,auth_method,mfa_level,expires_at,email_required,source_address,user_agent,last_seen_at)
		VALUES ($1,$2,$3,'local','none',$4,$5,$6,$7,clock_timestamp())`, orgID, sessionID, principalID, now.Add(m.policy.Idle),
		legacy, source.Unmap().String(), sessionUserAgent(userAgent)); err != nil {
		return Tokens{}, fmt.Errorf("create session: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_refresh_tokens (token_hash,organization_id,session_id)
		VALUES ($1,$2,$3)`, hash[:], orgID, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("create refresh token: %w", err)
	}
	action := "identity.login"
	if legacy {
		action = "identity.login_legacy_username"
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,$3,$4)`, orgID, principalID, action, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("audit login: %w", err)
	}
	access, expires, err := m.sign(now, orgID, principalID, sessionID, role)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, fmt.Errorf("commit session: %w", err)
	}
	return Tokens{Access: access, Refresh: refresh, AccessExpires: expires, SessionExpires: now.Add(m.policy.Idle),
		SessionID: sessionID, Organization: orgID, Principal: principalID, Role: role, EmailRequired: legacy}, nil
}

// sessionUserAgent keeps a printable, bounded User-Agent for the session list.
func sessionUserAgent(raw string) string {
	raw = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, strings.ToValidUTF8(raw, ""))
	for len(raw) > 256 {
		_, size := utf8.DecodeLastRuneInString(raw)
		raw = raw[:len(raw)-size]
	}
	return raw
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
	return m.liveSession(ctx, Caller{OrganizationID: claims.OrganizationID, PrincipalID: claims.Subject,
		SessionID: claims.SessionID, Role: claims.Role, AccessExpires: claims.ExpiresAt.Time})
}

// CheckSession revalidates a caller that was authenticated once by its access
// token (a long-lived stream or terminal socket): the session must still be
// live and the membership role unchanged. The access token's own expiry is
// not rechecked, so a stream outlives it while its session stays valid.
func (m *SessionManager) CheckSession(ctx context.Context, caller Caller) (Caller, error) {
	if m == nil || caller.OrganizationID == "" || caller.PrincipalID == "" || caller.SessionID == "" {
		return Caller{}, ErrUnauthenticated
	}
	return m.liveSession(ctx, caller)
}

func (m *SessionManager) liveSession(ctx context.Context, caller Caller) (Caller, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	var role string
	var emailRequired bool
	err := m.db.QueryRow(ctx, `SELECT m.role,s.email_required FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (s.credential_kind='service' OR o.mfa_policy<>'required' OR s.mfa_level='totp')`,
		caller.OrganizationID, caller.SessionID, caller.PrincipalID).Scan(&role, &emailRequired)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && role != caller.Role) {
		return Caller{}, ErrUnauthenticated
	}
	if err != nil {
		return Caller{}, fmt.Errorf("validate session: %w", err)
	}
	caller.Role, caller.EmailRequired = role, emailRequired
	return caller, nil
}

// Refresh rotates a refresh token. The rotated-out token stays answerable for
// refreshGrace: replaying it then returns the same successor (derived, not
// stored) and a fresh access token, provided that successor is still unused.
// Any other replay of a consumed token is theft-shaped and revokes the whole
// session. Each refresh slides the session's expiry to the idle timeout,
// capped at the absolute lifetime from sign-in.
func (m *SessionManager) Refresh(ctx context.Context, raw string) (Tokens, error) {
	ctx = tenant.System(ctx) // no organization is known until the credential is matched
	if m == nil {
		return Tokens{}, ErrUnauthenticated
	}
	hash, err := hashRefresh(raw)
	if err != nil {
		return Tokens{}, err
	}
	next, nextHash := m.successor(raw)
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
	// The session row lock serializes concurrent refreshes of one session.
	var principalID, authMethod, mfaLevel string
	var revoked *time.Time
	var created, expires time.Time
	var emailRequired bool
	err = tx.QueryRow(ctx, `SELECT principal_id,auth_method,mfa_level,created_at,expires_at,revoked_at,email_required
		FROM identity_sessions WHERE organization_id=$1 AND id=$2 FOR UPDATE`, orgID, sessionID).
		Scan(&principalID, &authMethod, &mfaLevel, &created, &expires, &revoked, &emailRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tokens{}, ErrUnauthenticated
	}
	if err != nil {
		return Tokens{}, fmt.Errorf("lock session: %w", err)
	}
	var consumed *time.Time
	var successorHash []byte
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT consumed_at,successor_hash,clock_timestamp() FROM identity_refresh_tokens
		WHERE token_hash=$1 FOR UPDATE`, hash[:]).Scan(&consumed, &successorHash, &now); err != nil {
		return Tokens{}, fmt.Errorf("lock refresh token: %w", err)
	}
	grace := false
	if consumed != nil {
		if now.Sub(*consumed) > refreshGrace || len(successorHash) != len(nextHash) {
			return Tokens{}, m.refreshReuse(ctx, tx, orgID, sessionID)
		}
		var unused bool
		err := tx.QueryRow(ctx, `SELECT consumed_at IS NULL FROM identity_refresh_tokens
			WHERE token_hash=$1 FOR UPDATE`, successorHash).Scan(&unused)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return Tokens{}, fmt.Errorf("lock refresh successor: %w", err)
		}
		if !unused {
			return Tokens{}, m.refreshReuse(ctx, tx, orgID, sessionID)
		}
		if subtle.ConstantTimeCompare(successorHash, nextHash[:]) != 1 {
			// Only a signer change inside the grace window derives differently;
			// the client signs in again and the session is not revoked.
			return Tokens{}, ErrUnauthenticated
		}
		grace = true
	}
	if revoked != nil || !expires.After(now) {
		return Tokens{}, ErrUnauthenticated
	}
	slid := now.Add(m.policy.Idle)
	if limit := created.Add(m.policy.Absolute); limit.Before(slid) {
		slid = limit
	}
	if !slid.After(now) {
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
	action := "identity.refresh_grace"
	if !grace {
		action = "identity.refresh"
		if _, err := tx.Exec(ctx, `UPDATE identity_refresh_tokens SET consumed_at=clock_timestamp(),successor_hash=$2
			WHERE token_hash=$1 AND consumed_at IS NULL`, hash[:], nextHash[:]); err != nil {
			return Tokens{}, fmt.Errorf("consume refresh token: %w", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_refresh_tokens (token_hash,organization_id,session_id)
			VALUES ($1,$2,$3)`, nextHash[:], orgID, sessionID); err != nil {
			return Tokens{}, fmt.Errorf("rotate refresh token: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_sessions SET last_seen_at=clock_timestamp(),expires_at=$3
		WHERE organization_id=$1 AND id=$2`, orgID, sessionID, slid); err != nil {
		return Tokens{}, fmt.Errorf("touch session: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,$3,$4)`, orgID, principalID, action, sessionID); err != nil {
		return Tokens{}, fmt.Errorf("audit refresh: %w", err)
	}
	access, accessExpires, err := m.sign(time.Now().UTC(), orgID, principalID, sessionID, role)
	if err != nil {
		return Tokens{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tokens{}, fmt.Errorf("commit refresh: %w", err)
	}
	return Tokens{Access: access, Refresh: next, AccessExpires: accessExpires, SessionExpires: slid, SessionID: sessionID,
		Organization: orgID, Principal: principalID, Role: role, EmailRequired: emailRequired}, nil
}

// refreshReuse revokes a session whose consumed refresh token was replayed.
func (m *SessionManager) refreshReuse(ctx context.Context, tx pgx.Tx, orgID, sessionID string) error {
	if _, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL`, orgID, sessionID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'system',NULL,'identity.refresh_reuse',$2)`, orgID, sessionID); err != nil {
		return fmt.Errorf("audit refresh reuse: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return ErrRefreshReuse
}

// successor derives the refresh token that replaces raw, which hashRefresh
// has already validated.
func (m *SessionManager) successor(raw string) (string, [32]byte) {
	secret, _ := base64.RawURLEncoding.DecodeString(raw)
	mac := hmac.New(sha256.New, m.successorKey)
	mac.Write(secret)
	clear(secret)
	next := mac.Sum(nil)
	text := base64.RawURLEncoding.EncodeToString(next)
	hash := sha256.Sum256(next)
	clear(next)
	return text, hash
}

func (m *SessionManager) Revoke(ctx context.Context, caller Caller) error {
	ctx = tenant.Org(ctx, caller.OrganizationID)
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

// RevokeRefresh supports logout even after the short access token expires.
func (m *SessionManager) RevokeRefresh(ctx context.Context, raw string) error {
	ctx = tenant.System(ctx) // no organization is known until the credential is matched
	if m == nil {
		return ErrUnauthenticated
	}
	hash, err := hashRefresh(raw)
	if err != nil {
		return err
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var orgID, sessionID, principalID string
	err = tx.QueryRow(ctx, `SELECT t.organization_id,t.session_id,s.principal_id
		FROM identity_refresh_tokens t JOIN identity_sessions s
		ON s.organization_id=t.organization_id AND s.id=t.session_id
		WHERE t.token_hash=$1 AND s.revoked_at IS NULL FOR UPDATE OF s`, hash[:]).
		Scan(&orgID, &sessionID, &principalID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthenticated
	}
	if err != nil {
		return fmt.Errorf("find logout session: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, orgID, sessionID); err != nil {
		return fmt.Errorf("revoke logout session: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'identity.logout',$3)`, orgID, principalID, sessionID); err != nil {
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

func hashRefresh(raw string) ([32]byte, error) {
	if len(raw) != 43 {
		return [32]byte{}, ErrUnauthenticated
	}
	secret, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(secret) != 32 {
		return [32]byte{}, ErrUnauthenticated
	}
	hash := sha256.Sum256(secret)
	clear(secret)
	return hash, nil
}
