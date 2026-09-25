package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var (
	ErrCurrentPassword = errors.New("current password is incorrect")
	ErrSessionNotFound = errors.New("session not found")
	ErrCurrentSession  = errors.New("use sign out to end this session")
)

// Profile is the caller's own account as shown in Account settings.
type Profile struct {
	PrincipalID, Email, DisplayName, Handle            string
	OrganizationID, OrganizationSlug, OrganizationName string
	Role                                               string
	EmailVerified, EmailRequired                       bool
	CreatedAt                                          time.Time
}

// Session is one of the caller's own sessions.
type Session struct {
	ID, OrganizationSlug, SourceAddress, UserAgent string
	Current                                        bool
	CreatedAt, ExpiresAt                           time.Time
	LastSeen                                       *time.Time
}

// ProfileChange carries only the fields being changed.
type ProfileChange struct {
	DisplayName, Email *string
	CurrentPassword    []byte
}

func selfCaller(caller Caller) error {
	if !validUUID(caller.OrganizationID) || !validUUID(caller.PrincipalID) || !validUUID(caller.SessionID) {
		return ErrUnauthenticated
	}
	return nil
}

// lockSelf fences a token whose session, membership, or principal changed
// since it was issued and locks the principal row for the change.
func lockSelf(ctx context.Context, tx pgx.Tx, caller Caller) (email *string, hash *string, err error) {
	if err := selfCaller(caller); err != nil {
		return nil, nil, err
	}
	err = tx.QueryRow(ctx, `SELECT p.email,p.password_hash FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3 AND $4::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m FOR UPDATE OF p`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.AccessExpires).Scan(&email, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, ErrUnauthenticated
	}
	return email, hash, err
}

// Profile reads the caller's own account.
func (m *SessionManager) Profile(ctx context.Context, caller Caller) (Profile, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	if err := selfCaller(caller); err != nil {
		return Profile{}, err
	}
	var p Profile
	err := m.db.QueryRow(ctx, `SELECT p.id,COALESCE(p.email,''),p.email_verified,p.display_name,p.username,
		o.id,o.slug,o.name,m.role,p.created_at,s.email_required
		FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp() AND m.state='active' AND p.state='active'`,
		caller.OrganizationID, caller.SessionID, caller.PrincipalID).Scan(&p.PrincipalID, &p.Email, &p.EmailVerified,
		&p.DisplayName, &p.Handle, &p.OrganizationID, &p.OrganizationSlug, &p.OrganizationName, &p.Role, &p.CreatedAt,
		&p.EmailRequired)
	if errors.Is(err, pgx.ErrNoRows) {
		return Profile{}, ErrUnauthenticated
	}
	return p, err
}

// checkCurrentPassword spends the per-principal reauthentication budget and
// one password slot, then verifies against the locked hash.
func (m *SessionManager) checkCurrentPassword(ctx context.Context, caller Caller, hash *string, password []byte) error {
	if err := m.limits.AllowReauth(ctx, caller.PrincipalID); err != nil {
		return err
	}
	select {
	case m.passwordSlots <- struct{}{}:
		defer func() { <-m.passwordSlots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if hash == nil {
		burnPasswordAttempt(password)
		return ErrCurrentPassword
	}
	if !VerifyPassword(*hash, password) {
		return ErrCurrentPassword
	}
	return nil
}

func revokeOtherSessions(ctx context.Context, tx pgx.Tx, caller Caller) (int64, error) {
	tag, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE principal_id=$1 AND NOT (organization_id=$2 AND id=$3) AND revoked_at IS NULL`,
		caller.PrincipalID, caller.OrganizationID, caller.SessionID)
	return tag.RowsAffected(), err
}

// UpdateProfile changes the caller's display name and/or email. An email
// change needs the current password, must be unused, is stored unverified,
// clears this session's email_required flag, and revokes every other session.
// A session that must set an email can change nothing else first.
func (m *SessionManager) UpdateProfile(ctx context.Context, caller Caller, change ProfileChange) (Profile, int64, error) {
	ctx = tenant.System(ctx) // the account and its sessions span organizations; queries filter by principal
	var displayName, email string
	if change.DisplayName != nil {
		displayName = strings.TrimSpace(*change.DisplayName)
		if !validDisplayName(displayName) {
			return Profile{}, 0, ErrUserInvalid
		}
	}
	if change.Email != nil {
		var err error
		if email, err = NormalizeEmail(*change.Email); err != nil {
			return Profile{}, 0, err
		}
	}
	if caller.EmailRequired && change.Email == nil {
		return Profile{}, 0, ErrEmailRequired
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return Profile{}, 0, err
	}
	defer tx.Rollback(ctx)
	current, hash, err := lockSelf(ctx, tx, caller)
	if err != nil {
		return Profile{}, 0, err
	}
	var revoked int64
	if change.Email != nil && (current == nil || *current != email) {
		if err := m.checkCurrentPassword(ctx, caller, hash, change.CurrentPassword); err != nil {
			return Profile{}, 0, err
		}
		if used, err := emailInUse(ctx, tx, email, caller.PrincipalID); err != nil {
			return Profile{}, 0, err
		} else if used {
			return Profile{}, 0, ErrEmailTaken
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_principals SET email=$2,email_verified=false WHERE id=$1`,
			caller.PrincipalID, email); err != nil {
			return Profile{}, 0, emailWriteError(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE identity_sessions SET email_required=false
			WHERE organization_id=$1 AND id=$2`, caller.OrganizationID, caller.SessionID); err != nil {
			return Profile{}, 0, err
		}
		if revoked, err = revokeOtherSessions(ctx, tx, caller); err != nil {
			return Profile{}, 0, err
		}
		from := ""
		if current != nil {
			from = *current
		}
		if err := audit(ctx, tx, caller, "identity.account.email_changed", caller.PrincipalID,
			map[string]string{"from": from, "to": email, "revoked_sessions": fmt.Sprint(revoked)}); err != nil {
			return Profile{}, 0, err
		}
	}
	if change.DisplayName != nil {
		tag, err := tx.Exec(ctx, `UPDATE identity_principals SET display_name=$2 WHERE id=$1 AND display_name<>$2`,
			caller.PrincipalID, displayName)
		if err != nil {
			return Profile{}, 0, err
		}
		if tag.RowsAffected() == 1 {
			if err := audit(ctx, tx, caller, "identity.account.profile_updated", caller.PrincipalID, nil); err != nil {
				return Profile{}, 0, err
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Profile{}, 0, err
	}
	caller.EmailRequired = false
	profile, err := m.Profile(ctx, caller)
	return profile, revoked, err
}

// ChangePassword sets a new password after checking the current one and signs
// out every other session. The new password follows the setup rules.
func (m *SessionManager) ChangePassword(ctx context.Context, caller Caller, currentPassword, newPassword []byte) (int64, error) {
	ctx = tenant.System(ctx) // the account and its sessions span organizations; queries filter by principal
	encoded, err := HashPassword(newPassword)
	if err != nil {
		return 0, err
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	_, hash, err := lockSelf(ctx, tx, caller)
	if err != nil {
		return 0, err
	}
	if err := m.checkCurrentPassword(ctx, caller, hash, currentPassword); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE identity_principals SET password_hash=$2 WHERE id=$1`, caller.PrincipalID, encoded); err != nil {
		return 0, err
	}
	// Open setup/reset links would otherwise still overwrite the new password.
	if _, err := tx.Exec(ctx, `UPDATE identity_account_links SET revoked_at=clock_timestamp()
		WHERE principal_id=$1 AND consumed_at IS NULL AND revoked_at IS NULL`, caller.PrincipalID); err != nil {
		return 0, err
	}
	revoked, err := revokeOtherSessions(ctx, tx, caller)
	if err != nil {
		return 0, err
	}
	if err := audit(ctx, tx, caller, "identity.account.password_changed", caller.PrincipalID,
		map[string]string{"revoked_sessions": fmt.Sprint(revoked)}); err != nil {
		return 0, err
	}
	return revoked, tx.Commit(ctx)
}

// ListSessions returns the caller's live sessions in every organization,
// newest first. ponytail: capped at 100; sessions expire within a week.
func (m *SessionManager) ListSessions(ctx context.Context, caller Caller) ([]Session, error) {
	ctx = tenant.System(ctx) // the account and its sessions span organizations; queries filter by principal
	if err := selfCaller(caller); err != nil {
		return nil, err
	}
	rows, err := m.db.Query(ctx, `SELECT s.id,o.slug,COALESCE(s.source_address,''),COALESCE(s.user_agent,''),
		s.organization_id=$2 AND s.id=$3,s.created_at,s.expires_at,s.last_seen_at
		FROM identity_sessions s JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.principal_id=$1 AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		ORDER BY s.created_at DESC LIMIT 100`, caller.PrincipalID, caller.OrganizationID, caller.SessionID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Session, error) {
		var s Session
		err := row.Scan(&s.ID, &s.OrganizationSlug, &s.SourceAddress, &s.UserAgent, &s.Current, &s.CreatedAt,
			&s.ExpiresAt, &s.LastSeen)
		return s, err
	})
}

// RevokeSession signs out one of the caller's other sessions. A session of
// anyone else is indistinguishable from one that does not exist.
func (m *SessionManager) RevokeSession(ctx context.Context, caller Caller, sessionID string) error {
	ctx = tenant.System(ctx) // the account and its sessions span organizations; queries filter by principal
	if !validUUID(sessionID) {
		return ErrSessionNotFound
	}
	if sessionID == caller.SessionID {
		return ErrCurrentSession
	}
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockSelf(ctx, tx, caller); err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE principal_id=$1 AND id=$2 AND revoked_at IS NULL AND expires_at>clock_timestamp()`, caller.PrincipalID, sessionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSessionNotFound
	}
	if err := audit(ctx, tx, caller, "identity.account.session_revoked", sessionID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RevokeOtherSessions signs the caller out everywhere but this session.
func (m *SessionManager) RevokeOtherSessions(ctx context.Context, caller Caller) (int64, error) {
	ctx = tenant.System(ctx) // the account and its sessions span organizations; queries filter by principal
	tx, err := m.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	if _, _, err := lockSelf(ctx, tx, caller); err != nil {
		return 0, err
	}
	revoked, err := revokeOtherSessions(ctx, tx, caller)
	if err != nil {
		return 0, err
	}
	if err := audit(ctx, tx, caller, "identity.account.other_sessions_revoked", caller.PrincipalID,
		map[string]string{"count": fmt.Sprint(revoked)}); err != nil {
		return 0, err
	}
	return revoked, tx.Commit(ctx)
}
