package db

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrate applies each embedded migration once and rejects edited migrations.
// A transaction-scoped advisory lock serializes migration and verification calls.
func Migrate(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	return runMigrations(ctx, pool, true)
}

// Verify rejects a missing, unknown, or edited migration without changing the schema.
func Verify(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := runMigrations(ctx, pool, false)
	return err
}

func runMigrations(ctx context.Context, pool *pgxpool.Pool, apply bool) (int, error) {
	if pool == nil {
		return 0, errors.New("database pool is required")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin migrations: %w", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('blaxsmith'), hashtext('schema'))`); err != nil {
		return 0, fmt.Errorf("lock migrations: %w", err)
	}
	// Migrations rewrite rows of every organization (0140_tenant_rls.sql).
	if _, err := tx.Exec(ctx, `SELECT set_config('blaxsmith.system', 'on', true)`); err != nil {
		return 0, fmt.Errorf("scope migrations: %w", err)
	}
	if apply {
		if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS blaxsmith_schema_migrations (
		version text PRIMARY KEY,
		sha256 text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT clock_timestamp()
	)`); err != nil {
			return 0, fmt.Errorf("create migration ledger: %w", err)
		}
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return 0, fmt.Errorf("list migrations: %w", err)
	}
	known := make(map[string]bool, len(names))
	for _, name := range names {
		known[strings.TrimSuffix(path.Base(name), ".sql")] = true
	}
	rows, err := tx.Query(ctx, `SELECT version FROM blaxsmith_schema_migrations`)
	if err != nil {
		return 0, fmt.Errorf("list applied migrations: %w", err)
	}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			rows.Close()
			return 0, fmt.Errorf("read applied migration: %w", err)
		}
		if !known[version] {
			rows.Close()
			return 0, fmt.Errorf("database migration %s is unknown to this binary", version)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read applied migrations: %w", err)
	}
	rows.Close()
	applied := 0
	for _, name := range names {
		body, err := migrations.ReadFile(name)
		if err != nil {
			return 0, fmt.Errorf("read %s: %w", name, err)
		}
		version := strings.TrimSuffix(path.Base(name), ".sql")
		digest := fmt.Sprintf("%x", sha256.Sum256(body))
		var recorded string
		err = tx.QueryRow(ctx, `SELECT sha256 FROM blaxsmith_schema_migrations WHERE version=$1`, version).Scan(&recorded)
		if err == nil {
			if recorded != digest {
				return 0, fmt.Errorf("migration %s checksum changed", version)
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return 0, fmt.Errorf("read migration %s: %w", version, err)
		}
		if !apply {
			return 0, fmt.Errorf("database migration %s is not applied", version)
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return 0, fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO blaxsmith_schema_migrations (version, sha256) VALUES ($1,$2)`, version, digest); err != nil {
			return 0, fmt.Errorf("record migration %s: %w", version, err)
		}
		applied++
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit migrations: %w", err)
	}
	return applied, nil
}
