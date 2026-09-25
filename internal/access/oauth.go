package access

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// CodexSubscriptionAuth marks a personal ChatGPT-plan Codex login. Its secret
// versions hold refresh material and are never delivered by native_raw.
const CodexSubscriptionAuth = "codex_chatgpt"

// ClaudeSetupTokenAuth marks a member's own `claude setup-token`: a long-lived
// Claude subscription token with no refresh material. It is delivered like an
// API key (native_raw), or kept on the platform and sent upstream by the
// model gateway as an OAuth bearer (brokered_gateway), but only for runs
// started by its owner, and only while
// the organization allows members' own Claude subscriptions
// (docs/model-gateway-plan.md §6.1).
const ClaudeSetupTokenAuth = "claude_setup_token"

const (
	codexTokenURL = "https://auth.openai.com/oauth/token"
	codexClientID = "app_EMoamEEZ73f0CkXaXp7hrann" // Codex CLI's public client (codex-rs/login).
	// Codex refreshes by itself when exp is within 5 minutes. Delivered tokens
	// must outlive the lease by more than that so the pod never tries.
	codexRefreshSkew = 10 * time.Minute
)

var (
	// ErrClaudeSubscriptionDisabled is a policy decision, not a missing
	// feature; see docs/subscription-auth.md.
	ErrClaudeSubscriptionDisabled = errors.New("your organization has not enabled members' own Claude subscriptions; " +
		"ask an owner or admin, or use an Anthropic API key")
	ErrReconnect   = errors.New("subscription login must be reconnected")
	ErrRateLimited = errors.New("subscription token endpoint rate limited")
)

type codexTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

func (t *codexTokens) clear() { *t = codexTokens{} }

// CodexAccount is the non-secret identity read from the pasted login.
type CodexAccount struct {
	AccountID, PlanType string
	AccessExpiresAt     time.Time
}

// ParseCodexAuth validates a pasted ~/.codex/auth.json from `codex login` or
// `codex login --device-auth`. Claims are read, not signature-verified: the
// account ID labels the owner's own connection and grants nothing by itself.
func ParseCodexAuth(raw []byte) (codexTokens, CodexAccount, error) {
	var file struct {
		APIKey *string      `json:"OPENAI_API_KEY"`
		Tokens *codexTokens `json:"tokens"`
	}
	if len(raw) == 0 || len(raw) > 12<<10 || json.Unmarshal(raw, &file) != nil || file.Tokens == nil ||
		(file.APIKey != nil && *file.APIKey != "") {
		return codexTokens{}, CodexAccount{}, ErrDenied
	}
	tokens := *file.Tokens
	account, err := codexAccount(tokens)
	if err != nil || tokens.RefreshToken == "" || len(tokens.RefreshToken) > 4096 ||
		strings.ContainsAny(tokens.RefreshToken, " \r\n\x00") {
		return codexTokens{}, CodexAccount{}, ErrDenied
	}
	tokens.AccountID = account.AccountID
	return tokens, account, nil
}

func codexAccount(tokens codexTokens) (CodexAccount, error) {
	var access struct {
		Exp int64 `json:"exp"`
	}
	var id struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if jwtClaims(tokens.AccessToken, &access) != nil || access.Exp <= 0 || jwtClaims(tokens.IDToken, &id) != nil {
		return CodexAccount{}, ErrDenied
	}
	accountID := tokens.AccountID
	if accountID == "" {
		accountID = id.Auth.AccountID
	}
	if accountID == "" || len(accountID) > 128 || strings.ContainsAny(accountID, "\r\n\x00") ||
		(id.Auth.AccountID != "" && id.Auth.AccountID != accountID) || len(id.Auth.PlanType) > 64 {
		return CodexAccount{}, ErrDenied
	}
	return CodexAccount{accountID, id.Auth.PlanType, time.Unix(access.Exp, 0)}, nil
}

func jwtClaims(token string, into any) error {
	parts := strings.Split(token, ".")
	if len(token) > 8192 || len(parts) != 3 {
		return ErrDenied
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ErrDenied
	}
	return json.Unmarshal(body, into)
}

// CreateCodexConnection stores a validated login as the owner's personal
// connection in the caller's transaction. Reconnect creates a new connection.
func CreateCodexConnection(ctx context.Context, tx pgx.Tx, secrets *SecretStore,
	organizationID, ownerID, providerRegistrationID string, raw []byte) (string, CodexAccount, error) {
	if tx == nil || secrets == nil || organizationID == "" || ownerID == "" || providerRegistrationID == "" {
		return "", CodexAccount{}, ErrDenied
	}
	tokens, account, err := ParseCodexAuth(raw)
	if err != nil {
		return "", CodexAccount{}, err
	}
	defer tokens.clear()
	bundle, err := json.Marshal(tokens)
	if err != nil {
		return "", CodexAccount{}, err
	}
	defer clear(bundle)
	var connectionID string
	err = tx.QueryRow(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,gen_random_uuid()::text,'user',$2,$3,$4,$5,'active') RETURNING id`,
		organizationID, ownerID, providerRegistrationID, account.AccountID, CodexSubscriptionAuth).Scan(&connectionID)
	if err != nil {
		return "", CodexAccount{}, fmt.Errorf("create subscription connection: %w", err)
	}
	version, err := secrets.RotateTx(ctx, tx, organizationID, connectionID, 0, bundle, nil)
	if err != nil {
		return "", CodexAccount{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_oauth_sessions
		(organization_id,connection_id,secret_version,access_expires_at) VALUES ($1,$2,$3,$4)`,
		organizationID, connectionID, version, account.AccessExpiresAt); err != nil {
		return "", CodexAccount{}, fmt.Errorf("create subscription session: %w", err)
	}
	return connectionID, account, nil
}

// OAuthRefresher is the only component that uses a subscription refresh
// token. Pods receive access tokens only.
type OAuthRefresher struct {
	DB       *pgxpool.Pool
	Secrets  *SecretStore
	Client   *http.Client // nil selects a 30-second client.
	TokenURL string       // empty selects the Codex production endpoint.

	// afterPersist, when set by a test, runs after a rotated token is
	// durable and before the session adopts it; an error simulates a crash.
	afterPersist func() error
}

// Delivery is what one attempt may receive: an access token, its provider
// expiry, and the native credential file with an empty refresh token.
type Delivery struct {
	AccessToken   []byte
	ExpiresAt     time.Time
	SecretVersion int64
	FileName      string // Relative to the harness HOME.
	File          []byte
	// AccountID is the ChatGPT account the model gateway names upstream
	// when it serves the owner's own run (docs/model-gateway-plan.md §6).
	AccountID string
}

func (d *Delivery) Clear() { clear(d.AccessToken); clear(d.File) }

// Deliver returns an access token valid past until plus the Codex refresh
// skew, refreshing first if needed. The caller must already have authorized
// the binding (AuthorizeModelInvoke) in its own transaction.
func (r *OAuthRefresher) Deliver(ctx context.Context, organizationID, connectionID string, until time.Time) (Delivery, error) {
	tokens, expiresAt, version, err := r.accessToken(ctx, organizationID, connectionID, until)
	if err != nil {
		return Delivery{}, err
	}
	defer tokens.clear()
	file, err := json.Marshal(map[string]any{
		"OPENAI_API_KEY": nil,
		// Codex requires the field to parse. Empty means the CLI cannot
		// refresh and so cannot rotate the shared login out from under its
		// siblings; a refresh attempt fails instead.
		"tokens": codexTokens{IDToken: tokens.IDToken, AccessToken: tokens.AccessToken,
			RefreshToken: "", AccountID: tokens.AccountID},
		// Now, so Codex's 8-day last_refresh fallback never triggers.
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return Delivery{}, err
	}
	return Delivery{AccessToken: []byte(tokens.AccessToken), ExpiresAt: expiresAt, SecretVersion: version,
		FileName: ".codex/auth.json", File: file, AccountID: tokens.AccountID}, nil
}

// OAuthLease names one delivered model lease being renewed until Until.
type OAuthLease struct {
	Invoke  ModelInvoke
	LeaseID string
	Until   time.Time
}

// RenewOAuthDelivery is the renewal-loop seam. In the caller's transaction it
// re-authorizes the frozen binding (current grant, policy, owner-only rule)
// and checks the delivered, unrevoked lease; the refresh itself commits in
// its own transaction first, so a rotated refresh token is durable before any
// access token leaves. The caller pushes Delivery.File to the guest and
// extends the lease no later than Delivery.ExpiresAt minus the Codex skew.
func (r *OAuthRefresher) RenewOAuthDelivery(ctx context.Context, tx pgx.Tx, lease OAuthLease) (Delivery, error) {
	if tx == nil || lease.LeaseID == "" || !lease.Until.After(time.Now()) {
		return Delivery{}, ErrDenied
	}
	decision, err := AuthorizeModelInvoke(ctx, tx, lease.Invoke)
	if err != nil {
		return Delivery{}, err
	}
	if decision.DeliveryMode != "oauth_access" {
		return Delivery{}, ErrDenied
	}
	var id string
	err = tx.QueryRow(ctx, `SELECT id FROM access_leases WHERE organization_id=$1 AND id=$2
		AND binding_id=$3 AND attempt_id=$4 AND connection_id=$5 AND capability='model.invoke'
		AND delivered_at IS NOT NULL AND revoked_at IS NULL AND expires_at>clock_timestamp()
		FOR SHARE`, lease.Invoke.OrganizationID, lease.LeaseID, lease.Invoke.BindingID,
		lease.Invoke.AttemptID, decision.ConnectionID).Scan(&id)
	if err != nil {
		return Delivery{}, deniedOrError("oauth lease", err)
	}
	return r.Deliver(ctx, lease.Invoke.OrganizationID, decision.ConnectionID, lease.Until)
}

// CodexLeaseMargin keeps a Codex lease this far inside its access token's
// expiry, so the CLI (which refreshes within 5 minutes of exp) never tries.
const CodexLeaseMargin = 5 * time.Minute

// RenewLease is the oauth_access renewal hook's database half. It rebuilds
// the frozen binding for a lease RenewModelLeases just extended, runs
// RenewOAuthDelivery in one transaction, and caps the lease at the delivered
// token's expiry minus CodexLeaseMargin. It returns the delivery and the
// lease expiry to announce to the guest.
func (r *OAuthRefresher) RenewLease(ctx context.Context, renewed RenewedLease) (Delivery, time.Time, error) {
	ctx = tenant.Org(ctx, renewed.OrganizationID)
	if r == nil || r.DB == nil || renewed.DeliveryMode != "oauth_access" || renewed.LeaseID == "" {
		return Delivery{}, time.Time{}, ErrDenied
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return Delivery{}, time.Time{}, err
	}
	defer tx.Rollback(ctx)
	invoke := ModelInvoke{OrganizationID: renewed.OrganizationID}
	var resource string
	err = tx.QueryRow(ctx, `SELECT b.project_id,b.attempt_id,b.id,g.grantee_kind,g.grantee_id,b.resource,b.policy_version
		FROM access_leases l
		JOIN access_bindings b ON b.organization_id=l.organization_id AND b.id=l.binding_id
		JOIN access_grants g ON g.organization_id=b.organization_id AND g.id=b.grant_id
		WHERE l.organization_id=$1 AND l.id=$2 AND l.attempt_id=$3 AND l.capability='model.invoke'`,
		renewed.OrganizationID, renewed.LeaseID, renewed.AttemptID).Scan(&invoke.ProjectID, &invoke.AttemptID,
		&invoke.BindingID, &invoke.GranteeKind, &invoke.GranteeID, &resource, &invoke.PolicyVersion)
	if err != nil {
		return Delivery{}, time.Time{}, deniedOrError("oauth lease binding", err)
	}
	var ok bool
	if invoke.Provider, invoke.Model, ok = strings.Cut(resource, "/"); !ok {
		return Delivery{}, time.Time{}, ErrDenied
	}
	delivery, err := r.RenewOAuthDelivery(ctx, tx, OAuthLease{Invoke: invoke, LeaseID: renewed.LeaseID, Until: renewed.ExpiresAt})
	if err != nil {
		return Delivery{}, time.Time{}, err
	}
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `UPDATE access_leases SET expires_at=LEAST(expires_at,$3)
		WHERE organization_id=$1 AND id=$2 RETURNING expires_at`,
		renewed.OrganizationID, renewed.LeaseID, delivery.ExpiresAt.Add(-CodexLeaseMargin)).Scan(&expiresAt)
	if err == nil {
		err = tx.Commit(ctx)
	}
	if err == nil && !expiresAt.After(time.Now()) {
		err = ErrDenied
	}
	if err != nil {
		delivery.Clear()
		return Delivery{}, time.Time{}, err
	}
	return delivery, expiresAt, nil
}

// accessToken serializes refresh per connection with a row lock on its
// session. Refresh tokens rotate on use, so only the lock holder may spend
// one. The rotated token set commits in its own short transaction as soon as
// the provider answers, so a crash before the session row adopts it cannot
// strand the connection: the next lock holder adopts the newest version.
func (r *OAuthRefresher) accessToken(ctx context.Context, organizationID, connectionID string,
	until time.Time) (codexTokens, time.Time, int64, error) {
	ctx = tenant.Org(ctx, organizationID)
	if r == nil || r.DB == nil || r.Secrets == nil || organizationID == "" || connectionID == "" {
		return codexTokens{}, time.Time{}, 0, ErrDenied
	}
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return codexTokens{}, time.Time{}, 0, err
	}
	defer tx.Rollback(ctx)
	var state, method string
	var version int64
	var expiresAt time.Time
	var reason *string
	err = tx.QueryRow(ctx, `SELECT c.state,c.auth_method,o.secret_version,o.access_expires_at,o.reconnect_reason
		FROM access_oauth_sessions o JOIN access_connections c
		ON c.organization_id=o.organization_id AND c.id=o.connection_id
		WHERE o.organization_id=$1 AND o.connection_id=$2 FOR UPDATE OF o`,
		organizationID, connectionID).Scan(&state, &method, &version, &expiresAt, &reason)
	if err != nil {
		return codexTokens{}, time.Time{}, 0, deniedOrError("oauth session", err)
	}
	if state != "active" || method != CodexSubscriptionAuth {
		return codexTokens{}, time.Time{}, 0, ErrDenied
	}
	if reason != nil {
		return codexTokens{}, time.Time{}, 0, ErrReconnect
	}
	// Only this refresher inserts versions past the session's, always under
	// this lock and only with provider-issued tokens.
	var newest int64
	if err := tx.QueryRow(ctx, `SELECT max(version) FROM access_secret_versions
		WHERE organization_id=$1 AND connection_id=$2`, organizationID, connectionID).Scan(&newest); err != nil {
		return codexTokens{}, time.Time{}, 0, err
	}
	secret, err := r.Secrets.readVersion(ctx, tx, organizationID, connectionID, newest)
	if err != nil {
		return codexTokens{}, time.Time{}, 0, err
	}
	var tokens codexTokens
	err = json.Unmarshal(secret.Bytes, &tokens)
	secret.Clear()
	if err != nil || tokens.RefreshToken == "" {
		return codexTokens{}, time.Time{}, 0, ErrDenied
	}
	if newest != version {
		// A previous refresh persisted this rotation and then died.
		account, err := codexAccount(tokens)
		if err != nil || account.AccountID != tokens.AccountID {
			tokens.clear()
			return codexTokens{}, time.Time{}, 0, ErrDenied
		}
		if _, err := tx.Exec(ctx, `UPDATE access_oauth_sessions SET secret_version=$3,access_expires_at=$4,
			refreshed_at=clock_timestamp() WHERE organization_id=$1 AND connection_id=$2`,
			organizationID, connectionID, newest, account.AccessExpiresAt); err != nil {
			tokens.clear()
			return codexTokens{}, time.Time{}, 0, err
		}
		version, expiresAt = newest, account.AccessExpiresAt
	}
	if expiresAt.After(until.Add(codexRefreshSkew)) {
		return tokens, expiresAt, version, tx.Commit(ctx)
	}
	// Hold the connection the rotation will commit on before spending the
	// token, so a full pool of waiters on this lock cannot strand it.
	persist, err := r.DB.Acquire(ctx)
	if err != nil {
		tokens.clear()
		return codexTokens{}, time.Time{}, 0, err
	}
	defer persist.Release()
	fresh, outcome := r.refresh(ctx, tokens)
	tokens.clear()
	if outcome != "" {
		if outcome == "rate_limited" {
			return codexTokens{}, time.Time{}, 0, ErrRateLimited
		}
		// A lost or rejected response may already have spent the refresh
		// token. Never retry it blindly; the owner reconnects.
		if _, err := tx.Exec(ctx, `UPDATE access_oauth_sessions SET reconnect_reason=$3
			WHERE organization_id=$1 AND connection_id=$2`, organizationID, connectionID, outcome); err != nil {
			return codexTokens{}, time.Time{}, 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return codexTokens{}, time.Time{}, 0, err
		}
		return codexTokens{}, time.Time{}, 0, ErrReconnect
	}
	account, err := codexAccount(fresh)
	if err != nil || account.AccountID != fresh.AccountID {
		fresh.clear()
		return codexTokens{}, time.Time{}, 0, ErrReconnect
	}
	bundle, err := json.Marshal(fresh)
	if err != nil {
		return codexTokens{}, time.Time{}, 0, err
	}
	defer clear(bundle)
	if err := r.persistRotation(ctx, persist, organizationID, connectionID, version+1, bundle); err != nil {
		fresh.clear()
		return codexTokens{}, time.Time{}, 0, err
	}
	if r.afterPersist != nil {
		if err := r.afterPersist(); err != nil {
			fresh.clear()
			return codexTokens{}, time.Time{}, 0, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE access_oauth_sessions SET secret_version=$3,access_expires_at=$4,
		refreshed_at=clock_timestamp() WHERE organization_id=$1 AND connection_id=$2`,
		organizationID, connectionID, version+1, account.AccessExpiresAt); err != nil {
		return codexTokens{}, time.Time{}, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return codexTokens{}, time.Time{}, 0, err
	}
	return fresh, account.AccessExpiresAt, version + 1, nil
}

// persistRotation commits a provider-issued token set right away, outside the
// session lock's transaction. It does not touch the session row, so it cannot
// wait on the lock its caller holds. A cancelled request still persists.
func (r *OAuthRefresher) persistRotation(ctx context.Context, conn *pgxpool.Conn, organizationID, connectionID string,
	version int64, bundle []byte) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.Secrets.insertVersion(ctx, tx, organizationID, connectionID, version, bundle, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// refresh returns a non-empty outcome for any failure: rate_limited,
// refresh_rejected (the provider answered no), or refresh_outcome_unknown.
func (r *OAuthRefresher) refresh(ctx context.Context, tokens codexTokens) (codexTokens, string) {
	body, _ := json.Marshal(map[string]string{"client_id": codexClientID, "grant_type": "refresh_token",
		"refresh_token": tokens.RefreshToken, "scope": "openid profile email"})
	defer clear(body)
	url := r.TokenURL
	if url == "" {
		url = codexTokenURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return codexTokens{}, "refresh_outcome_unknown"
	}
	req.Header.Set("Content-Type", "application/json")
	client := r.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return codexTokens{}, "refresh_outcome_unknown"
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return codexTokens{}, "rate_limited"
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized:
		return codexTokens{}, "refresh_rejected"
	case resp.StatusCode != http.StatusOK:
		return codexTokens{}, "refresh_outcome_unknown"
	}
	var fresh codexTokens
	if json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64<<10)).Decode(&fresh) != nil || fresh.AccessToken == "" {
		return codexTokens{}, "refresh_outcome_unknown"
	}
	if fresh.RefreshToken == "" {
		fresh.RefreshToken = tokens.RefreshToken
	}
	if fresh.IDToken == "" {
		fresh.IDToken = tokens.IDToken
	}
	fresh.AccountID = tokens.AccountID
	return fresh, ""
}
