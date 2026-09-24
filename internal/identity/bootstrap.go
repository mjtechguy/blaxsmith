package identity

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrBootstrapped = errors.New("installation already has identity data")
var ErrInvalidOwner = errors.New("invalid first-owner input")

var accountName = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,63}$`)
var organizationSlug = regexp.MustCompile(`^[a-z][a-z0-9-]{2,63}$`)

type FirstOwner struct {
	PrincipalID    string
	OrganizationID string
}

// BootstrapOwner is an operator-only entry point. Its one-time database
// invariant does not grant a public setup endpoint or bypass a later session.
// The owner signs in with email; the internal handle derives from it.
func BootstrapOwner(ctx context.Context, pool *pgxpool.Pool, email, slug, name string, password []byte) (FirstOwner, error) {
	email, emailErr := NormalizeEmail(email)
	if pool == nil || emailErr != nil || !organizationSlug.MatchString(slug) ||
		strings.TrimSpace(name) == "" || utf8.RuneCountInString(name) > 160 || !utf8.ValidString(name) ||
		strings.ContainsFunc(name, unicode.IsControl) {
		return FirstOwner{}, ErrInvalidOwner
	}
	hash, err := HashPassword(password)
	if err != nil {
		return FirstOwner{}, err
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return FirstOwner{}, fmt.Errorf("begin first-owner setup: %w", err)
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM identity_installation)
		OR EXISTS(SELECT 1 FROM identity_principals)
		OR EXISTS(SELECT 1 FROM identity_organizations)`).Scan(&exists); err != nil {
		return FirstOwner{}, fmt.Errorf("check first-owner state: %w", err)
	}
	if exists {
		return FirstOwner{}, ErrBootstrapped
	}
	username, err := freeHandle(ctx, tx, email)
	if err != nil {
		return FirstOwner{}, fmt.Errorf("allocate first-owner handle: %w", err)
	}
	var ownerID, orgID string
	if err := tx.QueryRow(ctx, `SELECT gen_random_uuid(), gen_random_uuid()`).Scan(&ownerID, &orgID); err != nil {
		return FirstOwner{}, fmt.Errorf("allocate first-owner IDs: %w", err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO identity_principals (id, username, email, password_hash) VALUES ($1,$2,$3,$4)`, []any{ownerID, username, email, hash}},
		{`INSERT INTO identity_organizations (id, slug, name) VALUES ($1,$2,$3)`, []any{orgID, slug, name}},
		{`INSERT INTO identity_memberships (organization_id, principal_id, role) VALUES ($1,$2,'owner')`, []any{orgID, ownerID}},
		{`INSERT INTO identity_installation (first_organization_id, first_owner_id) VALUES ($1,$2)`, []any{orgID, ownerID}},
		{`INSERT INTO identity_audit_events (organization_id, actor_kind, action, subject_id) VALUES ($1,'operator','installation.bootstrap_owner',$2)`, []any{orgID, ownerID}},
	} {
		if _, err := tx.Exec(ctx, statement.query, statement.args...); err != nil {
			return FirstOwner{}, bootstrapError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return FirstOwner{}, bootstrapError(err)
	}
	return FirstOwner{PrincipalID: ownerID, OrganizationID: orgID}, nil
}

func bootstrapError(err error) error {
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) && postgres.Code == "23505" {
		return ErrBootstrapped
	}
	return fmt.Errorf("first-owner setup: %w", err)
}
