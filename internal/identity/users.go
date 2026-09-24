package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrUserAdminDenied = errors.New("user administration denied")
	ErrOwnerOnly       = errors.New("only an owner can grant or change owner access")
	ErrLastOwner       = errors.New("the organization must keep an active owner who can sign in")
	ErrSelfDisable     = errors.New("you cannot disable your own account")
	ErrSharedAccount   = errors.New("account belongs to another organization")
	ErrUserExists      = errors.New("username already exists")
	ErrUserNotFound    = errors.New("member not found")
	ErrUserInvalid     = errors.New("invalid user input")
	ErrLinkInvalid     = errors.New("account link is invalid, used, or expired")
)

const (
	setupLinkLifetime = 72 * time.Hour
	resetLinkLifetime = 24 * time.Hour
)

var memberRoles = map[string]bool{"owner": true, "admin": true, "member": true, "viewer": true}

// UserAdmin manages organization members for owners and admins. Every
// mutation rechecks the caller's live session and role under a lock on the
// organization row, so concurrent role changes serialize and the last-owner
// rule cannot be raced. Changes revoke the target's sessions where access
// shrinks, and every change is written to identity_audit_events.
type UserAdmin struct{ db *pgxpool.Pool }

func NewUserAdmin(db *pgxpool.Pool) (*UserAdmin, error) {
	if db == nil {
		return nil, errors.New("user administration requires a database")
	}
	return &UserAdmin{db: db}, nil
}

// Member is a non-secret view of one organization membership. Status is
// invited (no password yet), active, or disabled.
type Member struct {
	PrincipalID, Username, DisplayName, Role, Status string
	ActiveSessions                                   int32
	LastLogin                                        *time.Time
	CreatedAt                                        time.Time
}

// AccountLink is a freshly issued setup or reset link. Token is returned
// exactly once and is never stored or logged in raw form.
type AccountLink struct {
	Token, Purpose, PrincipalID string
	ExpiresAt                   time.Time
}

// LinkInfo describes an open link to the person redeeming it.
type LinkInfo struct {
	Purpose, Username, DisplayName, OrganizationSlug, OrganizationName string
	ExpiresAt                                                          time.Time
}

func userAdminRole(caller Caller) error {
	if caller.Role != "owner" && caller.Role != "admin" {
		return ErrUserAdminDenied
	}
	if caller.OrganizationID == "" || caller.PrincipalID == "" || caller.SessionID == "" {
		return ErrUnauthenticated
	}
	return nil
}

// lockUserAdmin serializes membership changes per organization and fences a
// token whose session, membership, or role changed since it was issued.
func lockUserAdmin(ctx context.Context, tx pgx.Tx, caller Caller) error {
	if err := userAdminRole(caller); err != nil {
		return err
	}
	var id string
	if err := tx.QueryRow(ctx, `SELECT id FROM identity_organizations WHERE id=$1 FOR NO KEY UPDATE`,
		caller.OrganizationID).Scan(&id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		return err
	}
	var role string
	err := tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrUnauthenticated
	}
	return err
}

func audit(ctx context.Context, tx pgx.Tx, caller Caller, action, subject string, detail map[string]string) error {
	var payload any
	if detail != nil {
		payload = detail
	}
	_, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id,detail)
		VALUES ($1,'principal',$2,$3,$4,$5)`, caller.OrganizationID, caller.PrincipalID, action, subject, payload)
	return err
}

func validUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, r := range value {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdef", r) {
				return false
			}
		}
	}
	return true
}

// ListMembers returns every membership in the caller's organization, owners
// first. ponytail: unpaged, capped at 1000; page on the server when an
// organization outgrows it.
func (u *UserAdmin) ListMembers(ctx context.Context, caller Caller) ([]Member, error) {
	if err := userAdminRole(caller); err != nil {
		return nil, err
	}
	rows, err := u.db.Query(ctx, `SELECT p.id,p.username,p.display_name,m.role,
		CASE WHEN m.state='disabled' OR p.state='disabled' THEN 'disabled'
			WHEN p.password_hash IS NULL THEN 'invited' ELSE 'active' END,
		(SELECT count(*)::integer FROM identity_sessions s WHERE s.organization_id=m.organization_id
			AND s.principal_id=m.principal_id AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()),
		(SELECT max(s.created_at) FROM identity_sessions s WHERE s.organization_id=m.organization_id
			AND s.principal_id=m.principal_id),
		m.created_at
		FROM identity_memberships m JOIN identity_principals p ON p.id=m.principal_id
		WHERE m.organization_id=$1
		ORDER BY array_position(ARRAY['owner','admin','member','viewer'],m.role),p.username LIMIT 1000`,
		caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		err := row.Scan(&m.PrincipalID, &m.Username, &m.DisplayName, &m.Role, &m.Status, &m.ActiveSessions,
			&m.LastLogin, &m.CreatedAt)
		return m, err
	})
}

func validDisplayName(name string) bool {
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= 160 && !strings.ContainsFunc(name, unicode.IsControl)
}

// Invite creates a principal with no password, its membership, and a
// single-use setup link. Only an owner can invite an owner.
func (u *UserAdmin) Invite(ctx context.Context, caller Caller, username, displayName, role string) (AccountLink, error) {
	username, displayName = strings.ToLower(strings.TrimSpace(username)), strings.TrimSpace(displayName)
	if !accountName.MatchString(username) || !validDisplayName(displayName) || !memberRoles[role] {
		return AccountLink{}, ErrUserInvalid
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return AccountLink{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return AccountLink{}, err
	}
	if role == "owner" && caller.Role != "owner" {
		return AccountLink{}, ErrOwnerOnly
	}
	var principalID string
	if err := tx.QueryRow(ctx, `INSERT INTO identity_principals (id,username,display_name)
		VALUES (gen_random_uuid(),$1,$2) ON CONFLICT (username) DO NOTHING RETURNING id`, username, displayName).
		Scan(&principalID); errors.Is(err, pgx.ErrNoRows) {
		return AccountLink{}, ErrUserExists
	} else if err != nil {
		return AccountLink{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_memberships (organization_id,principal_id,role) VALUES ($1,$2,$3)`,
		caller.OrganizationID, principalID, role); err != nil {
		return AccountLink{}, err
	}
	link, err := issueLink(ctx, tx, caller, principalID, "setup")
	if err != nil {
		return AccountLink{}, err
	}
	if err := audit(ctx, tx, caller, "identity.user.invited", principalID, map[string]string{"role": role}); err != nil {
		return AccountLink{}, err
	}
	return link, tx.Commit(ctx)
}

func issueLink(ctx context.Context, tx pgx.Tx, caller Caller, principalID, purpose string) (AccountLink, error) {
	lifetime := setupLinkLifetime
	if purpose == "reset" {
		lifetime = resetLinkLifetime
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_account_links SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND principal_id=$2 AND consumed_at IS NULL AND revoked_at IS NULL`,
		caller.OrganizationID, principalID); err != nil {
		return AccountLink{}, err
	}
	token, hash, err := newRefresh()
	if err != nil {
		return AccountLink{}, err
	}
	var expires time.Time
	if err := tx.QueryRow(ctx, `INSERT INTO identity_account_links
		(token_hash,organization_id,principal_id,purpose,issued_by,expires_at)
		VALUES ($1,$2,$3,$4,$5,clock_timestamp()+$6::interval) RETURNING expires_at`,
		hash[:], caller.OrganizationID, principalID, purpose, caller.PrincipalID,
		fmt.Sprintf("%d seconds", int(lifetime.Seconds()))).Scan(&expires); err != nil {
		return AccountLink{}, err
	}
	return AccountLink{Token: token, Purpose: purpose, PrincipalID: principalID, ExpiresAt: expires}, nil
}

type target struct{ role, state string }

// lockTarget reads the member being changed and applies the owner rule: only
// an owner may change an owner.
func lockTarget(ctx context.Context, tx pgx.Tx, caller Caller, principalID string) (target, error) {
	if !validUUID(principalID) {
		return target{}, ErrUserInvalid
	}
	var t target
	err := tx.QueryRow(ctx, `SELECT role,state FROM identity_memberships
		WHERE organization_id=$1 AND principal_id=$2 FOR UPDATE`, caller.OrganizationID, principalID).Scan(&t.role, &t.state)
	if errors.Is(err, pgx.ErrNoRows) {
		return target{}, ErrUserNotFound
	}
	if err != nil {
		return target{}, err
	}
	if t.role == "owner" && caller.Role != "owner" {
		return target{}, ErrOwnerOnly
	}
	return t, nil
}

// requireAnotherOwner fails unless an active owner other than principalID can
// still sign in. An invited owner without a password does not count.
func requireAnotherOwner(ctx context.Context, tx pgx.Tx, org, principalID string) error {
	var others int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM identity_memberships m JOIN identity_principals p ON p.id=m.principal_id
		WHERE m.organization_id=$1 AND m.principal_id<>$2 AND m.role='owner' AND m.state='active'
		AND p.state='active' AND p.password_hash IS NOT NULL`, org, principalID).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return ErrLastOwner
	}
	return nil
}

func revokeSessions(ctx context.Context, tx pgx.Tx, org, principalID string) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND principal_id=$2 AND revoked_at IS NULL`, org, principalID)
	return tag.RowsAffected(), err
}

// SetRole changes a member's role and revokes their sessions so the new role
// applies at once. Owner is granted, and owners are changed, only by owners.
func (u *UserAdmin) SetRole(ctx context.Context, caller Caller, principalID, role string) error {
	if !memberRoles[role] {
		return ErrUserInvalid
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return err
	}
	t, err := lockTarget(ctx, tx, caller, principalID)
	if err != nil {
		return err
	}
	if role == "owner" && caller.Role != "owner" {
		return ErrOwnerOnly
	}
	if t.role == role {
		return nil
	}
	if t.role == "owner" {
		if err := requireAnotherOwner(ctx, tx, caller.OrganizationID, principalID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_memberships SET role=$3 WHERE organization_id=$1 AND principal_id=$2`,
		caller.OrganizationID, principalID, role); err != nil {
		return err
	}
	if _, err := revokeSessions(ctx, tx, caller.OrganizationID, principalID); err != nil {
		return err
	}
	if err := audit(ctx, tx, caller, "identity.user.role_changed", principalID, map[string]string{"from": t.role, "to": role}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetEnabled disables or re-enables a membership. Disabling revokes the
// member's sessions and any open setup/reset link.
func (u *UserAdmin) SetEnabled(ctx context.Context, caller Caller, principalID string, enabled bool) error {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return err
	}
	t, err := lockTarget(ctx, tx, caller, principalID)
	if err != nil {
		return err
	}
	state, action := "active", "identity.user.enabled"
	if !enabled {
		state, action = "disabled", "identity.user.disabled"
		if principalID == caller.PrincipalID {
			return ErrSelfDisable
		}
		if t.role == "owner" {
			if err := requireAnotherOwner(ctx, tx, caller.OrganizationID, principalID); err != nil {
				return err
			}
		}
	}
	if t.state == state {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_memberships SET state=$3 WHERE organization_id=$1 AND principal_id=$2`,
		caller.OrganizationID, principalID, state); err != nil {
		return err
	}
	if !enabled {
		if _, err := revokeSessions(ctx, tx, caller.OrganizationID, principalID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_account_links SET revoked_at=clock_timestamp()
			WHERE organization_id=$1 AND principal_id=$2 AND consumed_at IS NULL AND revoked_at IS NULL`,
			caller.OrganizationID, principalID); err != nil {
			return err
		}
	}
	if err := audit(ctx, tx, caller, action, principalID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// IssueReset issues a single-use link to set a new password (or to finish
// setup for a member who never set one). Passwords are installation-wide, so
// an admin cannot reset an account that also belongs to another organization.
func (u *UserAdmin) IssueReset(ctx context.Context, caller Caller, principalID string) (AccountLink, error) {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return AccountLink{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return AccountLink{}, err
	}
	t, err := lockTarget(ctx, tx, caller, principalID)
	if err != nil {
		return AccountLink{}, err
	}
	var shared, hasPassword, active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_memberships WHERE principal_id=$2 AND organization_id<>$1),
		password_hash IS NOT NULL, state='active' FROM identity_principals WHERE id=$2 FOR UPDATE`,
		caller.OrganizationID, principalID).Scan(&shared, &hasPassword, &active); err != nil {
		return AccountLink{}, err
	}
	if shared {
		return AccountLink{}, ErrSharedAccount
	}
	if t.state != "active" || !active {
		return AccountLink{}, ErrUserInvalid
	}
	purpose := "reset"
	if !hasPassword {
		purpose = "setup"
	}
	link, err := issueLink(ctx, tx, caller, principalID, purpose)
	if err != nil {
		return AccountLink{}, err
	}
	if err := audit(ctx, tx, caller, "identity.user."+purpose+"_link_issued", principalID, nil); err != nil {
		return AccountLink{}, err
	}
	return link, tx.Commit(ctx)
}

// RevokeSessions signs a member out of every session in this organization.
func (u *UserAdmin) RevokeSessions(ctx context.Context, caller Caller, principalID string) (int64, error) {
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return 0, err
	}
	if _, err := lockTarget(ctx, tx, caller, principalID); err != nil {
		return 0, err
	}
	count, err := revokeSessions(ctx, tx, caller.OrganizationID, principalID)
	if err != nil {
		return 0, err
	}
	if err := audit(ctx, tx, caller, "identity.user.sessions_revoked", principalID, map[string]string{"count": fmt.Sprint(count)}); err != nil {
		return 0, err
	}
	return count, tx.Commit(ctx)
}

func hashLink(raw string) ([32]byte, error) {
	hash, err := hashRefresh(raw)
	if err != nil {
		return [32]byte{}, ErrLinkInvalid
	}
	return hash, nil
}

const openLink = `SELECT l.purpose,p.username,p.display_name,o.slug,o.name,l.expires_at,l.organization_id,l.principal_id
	FROM identity_account_links l
	JOIN identity_memberships m ON m.organization_id=l.organization_id AND m.principal_id=l.principal_id
	JOIN identity_principals p ON p.id=l.principal_id
	JOIN identity_organizations o ON o.id=l.organization_id
	WHERE l.token_hash=$1 AND l.consumed_at IS NULL AND l.revoked_at IS NULL AND l.expires_at>clock_timestamp()
	AND m.state='active' AND p.state='active'`

// InspectLink is public: it returns who the link is for, or ErrLinkInvalid.
func (u *UserAdmin) InspectLink(ctx context.Context, token string) (LinkInfo, error) {
	hash, err := hashLink(token)
	if err != nil {
		return LinkInfo{}, err
	}
	var info LinkInfo
	var org, principal string
	err = u.db.QueryRow(ctx, openLink, hash[:]).Scan(&info.Purpose, &info.Username, &info.DisplayName,
		&info.OrganizationSlug, &info.OrganizationName, &info.ExpiresAt, &org, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkInfo{}, ErrLinkInvalid
	}
	return info, err
}

// CompleteLink sets the member's password, consumes the link, and revokes
// every existing session of that principal. The password is hashed only after
// the link is known to be open, so invalid tokens cost no Argon2 work.
func (u *UserAdmin) CompleteLink(ctx context.Context, token string, password []byte) (LinkInfo, error) {
	if _, err := u.InspectLink(ctx, token); err != nil {
		return LinkInfo{}, err
	}
	encoded, err := HashPassword(password)
	if err != nil {
		return LinkInfo{}, err
	}
	hash, _ := hashLink(token)
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return LinkInfo{}, err
	}
	defer tx.Rollback(ctx)
	var info LinkInfo
	var org, principal string
	err = tx.QueryRow(ctx, openLink+` FOR UPDATE OF l,p`, hash[:]).Scan(&info.Purpose, &info.Username, &info.DisplayName,
		&info.OrganizationSlug, &info.OrganizationName, &info.ExpiresAt, &org, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkInfo{}, ErrLinkInvalid
	}
	if err != nil {
		return LinkInfo{}, err
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE identity_account_links SET consumed_at=clock_timestamp() WHERE token_hash=$1`, []any{hash[:]}},
		{`UPDATE identity_principals SET password_hash=$2 WHERE id=$1`, []any{principal, encoded}},
		{`UPDATE identity_sessions SET revoked_at=clock_timestamp() WHERE principal_id=$1 AND revoked_at IS NULL`, []any{principal}},
		{`INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
			VALUES ($1,'principal',$2,$3,$2)`, []any{org, principal, "identity.account." + info.Purpose + "_completed"}},
	} {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return LinkInfo{}, err
		}
	}
	return info, tx.Commit(ctx)
}
