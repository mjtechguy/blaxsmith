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
	Email                                            string
	EmailVerified                                    bool
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
	Email                                                              string
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
	rows, err := u.db.Query(ctx, `SELECT p.id,p.username,p.display_name,COALESCE(p.email,''),p.email_verified,m.role,
		CASE WHEN m.state='disabled' OR p.state='disabled' THEN 'disabled'
			WHEN p.password_hash IS NULL THEN 'invited' ELSE 'active' END,
		(SELECT count(*)::integer FROM identity_sessions s WHERE s.organization_id=m.organization_id
			AND s.principal_id=m.principal_id AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()),
		(SELECT max(s.created_at) FROM identity_sessions s WHERE s.organization_id=m.organization_id
			AND s.principal_id=m.principal_id),
		m.created_at
		FROM identity_memberships m JOIN identity_principals p ON p.id=m.principal_id
		WHERE m.organization_id=$1
		ORDER BY array_position(ARRAY['owner','admin','member','viewer'],m.role),COALESCE(p.email,p.username) LIMIT 1000`,
		caller.OrganizationID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		err := row.Scan(&m.PrincipalID, &m.Username, &m.DisplayName, &m.Email, &m.EmailVerified, &m.Role, &m.Status,
			&m.ActiveSessions, &m.LastLogin, &m.CreatedAt)
		return m, err
	})
}

// MemberFilter pages the member directory with a 1-based Page; zero values
// take defaults (page 1, 25 rows, role order ascending).
type MemberFilter struct {
	Page, PageSize        int
	Search                string
	Roles, Statuses       []string
	SortBy, SortDirection string
}

var memberSorts = map[string]string{
	"role":       "array_position(ARRAY['owner','admin','member','viewer'],role)",
	"username":   "username",
	"email":      "email",
	"last_login": "last_login",
	"created_at": "created_at",
}

// ListMembersPage is the paged, filtered form of ListMembers with the same
// status, session, and last-login semantics, plus the filtered total.
func (u *UserAdmin) ListMembersPage(ctx context.Context, caller Caller, f MemberFilter) ([]Member, int32, error) {
	if err := userAdminRole(caller); err != nil {
		return nil, 0, err
	}
	if f.Page == 0 {
		f.Page = 1
	}
	if f.PageSize == 0 {
		f.PageSize = 25
	}
	if f.SortBy == "" {
		f.SortBy = "role"
	}
	if f.SortDirection == "" {
		f.SortDirection = "asc"
	}
	f.Search = strings.TrimSpace(f.Search)
	order, ok := memberSorts[f.SortBy]
	if !ok || f.Page < 1 || f.PageSize < 1 || f.PageSize > 100 || (f.Page-1)*f.PageSize > 10000 ||
		len(f.Search) > 120 || (f.SortDirection != "asc" && f.SortDirection != "desc") ||
		len(f.Roles) > len(memberRoles) || len(f.Statuses) > 3 {
		return nil, 0, ErrUserInvalid
	}
	for _, role := range f.Roles {
		if !memberRoles[role] {
			return nil, 0, ErrUserInvalid
		}
	}
	for _, status := range f.Statuses {
		if status != "invited" && status != "active" && status != "disabled" {
			return nil, 0, ErrUserInvalid
		}
	}
	roles, statuses := append([]string{}, f.Roles...), append([]string{}, f.Statuses...)
	order += " " + f.SortDirection
	if f.SortBy == "role" {
		order += ",email " + f.SortDirection
	} else if f.SortBy == "last_login" {
		order += " NULLS LAST"
	}
	const filtered = `WITH members AS (SELECT p.id,p.username,p.display_name,COALESCE(p.email,'') AS email,p.email_verified,m.role,
		CASE WHEN m.state='disabled' OR p.state='disabled' THEN 'disabled'
			WHEN p.password_hash IS NULL THEN 'invited' ELSE 'active' END AS status,
		(SELECT count(*)::integer FROM identity_sessions s WHERE s.organization_id=m.organization_id
			AND s.principal_id=m.principal_id AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()) AS sessions,
		(SELECT max(s.created_at) FROM identity_sessions s WHERE s.organization_id=m.organization_id
			AND s.principal_id=m.principal_id) AS last_login,
		m.created_at
		FROM identity_memberships m JOIN identity_principals p ON p.id=m.principal_id
		WHERE m.organization_id=$1 AND (cardinality($2::text[])=0 OR m.role=ANY($2::text[]))
		AND ($3='' OR position(lower($3) in lower(COALESCE(p.email,'')||' '||p.username||' '||p.display_name))>0)),
	filtered AS (SELECT * FROM members WHERE cardinality($4::text[])=0 OR status=ANY($4::text[]))`
	args := []any{caller.OrganizationID, roles, f.Search, statuses}
	var total int32
	if err := u.db.QueryRow(ctx, filtered+` SELECT count(*)::integer FROM filtered`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := u.db.Query(ctx, filtered+` SELECT id,username,display_name,email,email_verified,role,status,sessions,last_login,created_at
		FROM filtered ORDER BY `+order+`,id `+f.SortDirection+` LIMIT $5 OFFSET $6`,
		append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	if err != nil {
		return nil, 0, err
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Member, error) {
		var m Member
		err := row.Scan(&m.PrincipalID, &m.Username, &m.DisplayName, &m.Email, &m.EmailVerified, &m.Role, &m.Status,
			&m.ActiveSessions, &m.LastLogin, &m.CreatedAt)
		return m, err
	})
	return members, total, err
}

func validDisplayName(name string) bool {
	return utf8.ValidString(name) && utf8.RuneCountInString(name) <= 160 && !strings.ContainsFunc(name, unicode.IsControl)
}

// Invite creates a principal with the given email and no password, its
// membership, and a single-use setup link. Only an owner can invite an owner.
// The internal handle is derived from the email.
func (u *UserAdmin) Invite(ctx context.Context, caller Caller, email, displayName, role string) (AccountLink, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return AccountLink{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if !validDisplayName(displayName) || !memberRoles[role] {
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
	if used, err := emailInUse(ctx, tx, email, ""); err != nil {
		return AccountLink{}, err
	} else if used {
		return AccountLink{}, emailTakenIn(ctx, tx, caller.OrganizationID, email)
	}
	username, err := freeHandle(ctx, tx, email)
	if err != nil {
		return AccountLink{}, err
	}
	var principalID string
	if err := tx.QueryRow(ctx, `INSERT INTO identity_principals (id,username,email,display_name)
		VALUES (gen_random_uuid(),$1,$2,$3) RETURNING id`, username, email, displayName).
		Scan(&principalID); err != nil {
		return AccountLink{}, hideTaken(emailWriteError(err))
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

const openLink = `SELECT l.purpose,p.username,p.display_name,COALESCE(p.email,''),o.slug,o.name,l.expires_at,l.organization_id,l.principal_id
	FROM identity_account_links l
	JOIN identity_memberships m ON m.organization_id=l.organization_id AND m.principal_id=l.principal_id
	JOIN identity_principals p ON p.id=l.principal_id
	JOIN identity_organizations o ON o.id=l.organization_id
	WHERE l.token_hash=$1 AND l.consumed_at IS NULL AND l.revoked_at IS NULL AND l.expires_at>clock_timestamp()
	AND m.state='active' AND p.state='active'`

// AllowLink spends the shared link budget for one public link request from
// peer, the connection's real client address (never a client header).
func (u *UserAdmin) AllowLink(ctx context.Context, token, peer string) error {
	source, err := peerAddress(peer)
	if err != nil {
		return err
	}
	return (&LoginLimit{db: u.db}).AllowLink(ctx, token, source)
}

// InspectLink is public: it returns who the link is for, or ErrLinkInvalid.
func (u *UserAdmin) InspectLink(ctx context.Context, token string) (LinkInfo, error) {
	hash, err := hashLink(token)
	if err != nil {
		return LinkInfo{}, err
	}
	var info LinkInfo
	var org, principal string
	err = u.db.QueryRow(ctx, openLink, hash[:]).Scan(&info.Purpose, &info.Username, &info.DisplayName, &info.Email,
		&info.OrganizationSlug, &info.OrganizationName, &info.ExpiresAt, &org, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkInfo{}, ErrLinkInvalid
	}
	return info, err
}

// CompleteLink sets the member's password, consumes the link, and revokes
// every existing session of that principal. The password is hashed only after
// the link is known to be open, so invalid tokens cost no Argon2 work. A
// non-empty displayName replaces the current one; email is used only when the
// account has none yet (an account created before emails were required).
func (u *UserAdmin) CompleteLink(ctx context.Context, token string, password []byte, displayName, email string) (LinkInfo, error) {
	opened, err := u.InspectLink(ctx, token)
	if err != nil {
		return LinkInfo{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if !validDisplayName(displayName) {
		return LinkInfo{}, ErrUserInvalid
	}
	if opened.Email == "" {
		if email, err = NormalizeEmail(email); err != nil {
			return LinkInfo{}, err
		}
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
	err = tx.QueryRow(ctx, openLink+` FOR UPDATE OF l,p`, hash[:]).Scan(&info.Purpose, &info.Username, &info.DisplayName, &info.Email,
		&info.OrganizationSlug, &info.OrganizationName, &info.ExpiresAt, &org, &principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return LinkInfo{}, ErrLinkInvalid
	}
	if err != nil {
		return LinkInfo{}, err
	}
	if info.Email == "" {
		if email == "" {
			return LinkInfo{}, ErrEmailInvalid
		}
		if used, err := emailInUse(ctx, tx, email, principal); err != nil {
			return LinkInfo{}, err
		} else if used {
			return LinkInfo{}, ErrEmailInvalid // Public path: a taken address reads as invalid (see UpdateProfile).
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_principals SET email=$2,email_verified=false WHERE id=$1`, principal, email); err != nil {
			return LinkInfo{}, hideTaken(emailWriteError(err))
		}
		info.Email = email
	}
	if displayName != "" {
		info.DisplayName = displayName
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE identity_account_links SET consumed_at=clock_timestamp() WHERE token_hash=$1`, []any{hash[:]}},
		{`UPDATE identity_principals SET password_hash=$2,display_name=$3 WHERE id=$1`, []any{principal, encoded, info.DisplayName}},
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

// SetEmail is the owner/admin repair of a member's sign-in email. It follows
// IssueReset's rules (an owner is changed only by an owner; an account shared
// with another organization is refused), stores the email unverified, and
// revokes the member's sessions.
func (u *UserAdmin) SetEmail(ctx context.Context, caller Caller, principalID, email string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	tx, err := u.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockUserAdmin(ctx, tx, caller); err != nil {
		return err
	}
	if _, err := lockTarget(ctx, tx, caller, principalID); err != nil {
		return err
	}
	var shared bool
	var current *string
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_memberships WHERE principal_id=$2 AND organization_id<>$1),
		email FROM identity_principals WHERE id=$2 FOR UPDATE`, caller.OrganizationID, principalID).Scan(&shared, &current); err != nil {
		return err
	}
	if shared {
		return ErrSharedAccount
	}
	if current != nil && *current == email {
		return nil
	}
	if used, err := emailInUse(ctx, tx, email, principalID); err != nil {
		return err
	} else if used {
		return emailTakenIn(ctx, tx, caller.OrganizationID, email)
	}
	count, err := setEmail(ctx, tx, principalID, email)
	if err != nil {
		return hideTaken(err)
	}
	if err := audit(ctx, tx, caller, "identity.user.email_changed", principalID, emailChange(current, email, count)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func emailChange(from *string, to string, revoked int64) map[string]string {
	detail := map[string]string{"from": "", "to": to, "revoked_sessions": fmt.Sprint(revoked)}
	if from != nil {
		detail["from"] = *from
	}
	return detail
}

// setEmail writes a checked, unused email and revokes every session of the
// principal, returning how many were revoked.
func setEmail(ctx context.Context, tx pgx.Tx, principalID, email string) (int64, error) {
	if used, err := emailInUse(ctx, tx, email, principalID); err != nil {
		return 0, err
	} else if used {
		return 0, ErrEmailTaken
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_principals SET email=$2,email_verified=false WHERE id=$1`, principalID, email); err != nil {
		return 0, emailWriteError(err)
	}
	tag, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE principal_id=$1 AND revoked_at IS NULL`, principalID)
	return tag.RowsAffected(), err
}

// OperatorSetEmail is the restricted operator repair behind `blaxsmith admin
// set-email`: it finds the principal by current email or handle, sets the new
// email as SetEmail does, revokes its sessions, and audits the change as the
// operator in every organization the principal belongs to.
func OperatorSetEmail(ctx context.Context, pool *pgxpool.Pool, login, email string) (string, error) {
	email, err := NormalizeEmail(email)
	if err != nil {
		return "", err
	}
	login = strings.ToLower(strings.TrimSpace(login))
	if pool == nil || login == "" {
		return "", ErrUserNotFound
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var principalID string
	var current *string
	err = tx.QueryRow(ctx, `SELECT id,email FROM identity_principals WHERE lower(email)=$1 OR username=$1
		ORDER BY lower(email)=$1 DESC NULLS LAST LIMIT 1 FOR UPDATE`, login).Scan(&principalID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrUserNotFound
	}
	if err != nil {
		return "", err
	}
	if current != nil && *current == email {
		return principalID, nil
	}
	count, err := setEmail(ctx, tx, principalID, email)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,action,subject_id,detail)
		SELECT organization_id,'operator','identity.user.email_changed',principal_id,$2
		FROM identity_memberships WHERE principal_id=$1`, principalID, emailChange(current, email, count)); err != nil {
		return "", err
	}
	return principalID, tx.Commit(ctx)
}
