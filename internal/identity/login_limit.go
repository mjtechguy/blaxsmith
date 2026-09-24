package identity

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrRateLimited = errors.New("too many login attempts")

// LoginLimit uses PostgreSQL row locks so attempts count across API replicas.
// The source must come from the trusted network boundary, never a client header.
type LoginLimit struct{ db *pgxpool.Pool }

func NewLoginLimit(db *pgxpool.Pool) (*LoginLimit, error) {
	if db == nil {
		return nil, errors.New("login limit requires a database")
	}
	return &LoginLimit{db: db}, nil
}

// login is the normalized sign-in identifier (an email, or a legacy
// username). Emails are installation-wide, so the organization is not part of
// any key: choosing a different organization cannot reset the account budget.
func (l *LoginLimit) Allow(ctx context.Context, login string, source netip.Addr) error {
	if l == nil || !source.IsValid() || login == "" || len(login) > 254 {
		return ErrUnauthenticated
	}
	address := source.Unmap().String()
	for _, item := range []struct {
		scope string
		key   string
		max   int
	}{
		{"source", address, 60},
		{"account_source", login + "\x00" + address, 10},
		{"account", login, 30},
	} {
		hash := sha256.Sum256([]byte(item.scope + "\x00" + item.key))
		var allowed bool
		err := l.db.QueryRow(ctx, `INSERT INTO identity_login_limits
			(scope,key_hash,window_start,attempts)
			VALUES ($1,$2,clock_timestamp(),1)
			ON CONFLICT (scope,key_hash) DO UPDATE SET
			window_start=CASE WHEN identity_login_limits.window_start <= EXCLUDED.window_start - interval '1 minute'
				THEN EXCLUDED.window_start ELSE identity_login_limits.window_start END,
			attempts=CASE WHEN identity_login_limits.window_start <= EXCLUDED.window_start - interval '1 minute'
				THEN 1 ELSE LEAST(identity_login_limits.attempts + 1,$3 + 1) END
			RETURNING attempts <= $3`, item.scope, hash[:], item.max).Scan(&allowed)
		if err != nil {
			return fmt.Errorf("check login limit: %w", err)
		}
		if !allowed {
			return ErrRateLimited
		}
	}
	return nil
}

// AllowReauth budgets current-password checks on account changes per
// principal, separately from sign-in, so a stolen session cannot brute-force
// the password it would need to take the account over.
func (l *LoginLimit) AllowReauth(ctx context.Context, principalID string) error {
	if l == nil || !validUUID(principalID) {
		return ErrUnauthenticated
	}
	hash := sha256.Sum256([]byte("reauth\x00" + principalID))
	var allowed bool
	err := l.db.QueryRow(ctx, `INSERT INTO identity_login_limits (scope,key_hash,window_start,attempts)
		VALUES ('reauth',$1,clock_timestamp(),1)
		ON CONFLICT (scope,key_hash) DO UPDATE SET
		window_start=CASE WHEN identity_login_limits.window_start <= EXCLUDED.window_start - interval '15 minutes'
			THEN EXCLUDED.window_start ELSE identity_login_limits.window_start END,
		attempts=CASE WHEN identity_login_limits.window_start <= EXCLUDED.window_start - interval '15 minutes'
			THEN 1 ELSE LEAST(identity_login_limits.attempts + 1,11) END
		RETURNING attempts <= 10`, hash[:]).Scan(&allowed)
	if err != nil {
		return fmt.Errorf("check reauthentication limit: %w", err)
	}
	if !allowed {
		return ErrRateLimited
	}
	return nil
}

// Prune removes old, no-longer-active limit keys. Run from platform maintenance.
func (l *LoginLimit) Prune(ctx context.Context) error {
	if l == nil {
		return errors.New("login limit is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err := l.db.Exec(ctx, `DELETE FROM identity_login_limits
		WHERE window_start < clock_timestamp() - interval '1 day'`)
	return err
}
