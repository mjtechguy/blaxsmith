package identity

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var ErrEmailInvalid = errors.New("enter a valid email address")
var ErrEmailTaken = errors.New("that email is already in use")

// NormalizeEmail trims and lowercases an address and accepts only a bare
// addr-spec (no display name or comments) with a dotted domain.
func NormalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if len(email) < 3 || len(email) > 254 || strings.ContainsAny(email, " \t\r\n\"<>()[],;:\\") {
		return "", ErrEmailInvalid
	}
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || address.Name != "" {
		return "", ErrEmailInvalid
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || len(local) > 64 || strings.Contains(domain, "@") || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", ErrEmailInvalid
	}
	return email, nil
}

// handleBase derives an internal handle from an email's local part that fits
// the username rule: a leading letter, then [a-z0-9._-], 3 to 64 bytes.
func handleBase(email string) string {
	local, _, _ := strings.Cut(email, "@")
	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == '+':
			b.WriteByte('-')
		}
	}
	handle := strings.TrimLeft(b.String(), "0123456789._-")
	if handle == "" {
		handle = "user"
	}
	for len(handle) < 3 {
		handle += "0"
	}
	if len(handle) > 56 {
		handle = handle[:56]
	}
	return handle
}

// freeHandle returns an unused handle for email, adding -2, -3, ... when the
// base is taken. The unique constraint still guards a concurrent insert.
func freeHandle(ctx context.Context, tx pgx.Tx, email string) (string, error) {
	base := handleBase(email)
	for n := 1; n < 1000; n++ {
		candidate := base
		if n > 1 {
			candidate = fmt.Sprintf("%s-%d", base, n)
		}
		var taken bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_principals WHERE username=$1)`, candidate).
			Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return candidate, nil
		}
	}
	return "", ErrUserExists
}

// emailInUse reports whether another principal already holds email.
func emailInUse(ctx context.Context, tx pgx.Tx, email, except string) (bool, error) {
	var used bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_principals
		WHERE lower(email)=lower($1) AND id::text<>$2)`, email, except).Scan(&used)
	return used, err
}

// emailWriteError maps a lost uniqueness race or a rejected address.
func emailWriteError(err error) error {
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		switch postgres.Code {
		case "23505":
			return ErrEmailTaken
		case "23514":
			return ErrEmailInvalid
		}
	}
	return err
}
