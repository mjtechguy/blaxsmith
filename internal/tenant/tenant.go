// Package tenant carries the database tenancy scope that row-level security
// (db/migrations/0140_tenant_rls.sql) enforces. A context is scoped to one
// organization with Org, or to cross-organization system work with System; an
// unscoped context sees no organization-owned rows and cannot write them.
//
// Scope is applied when a connection is acquired from a pool configured with
// Configure: every acquisition overwrites both settings from the acquiring
// context, so nothing carries over between callers sharing a connection. A
// transaction keeps the scope of the context that began it.
package tenant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type scopeKey struct{}

type scope struct {
	org    string
	system bool
}

// Org scopes ctx to organizationID. The innermost scope wins, so a store method
// that knows its organization pins it regardless of what its caller set.
func Org(ctx context.Context, organizationID string) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{org: organizationID})
}

// System scopes ctx to cross-organization work: background loops, login before
// an organization is known, token lookups, installation administration, and
// migrations. Use it only where no single organization owns the work.
func System(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope{system: true})
}

// Configure installs the per-acquisition scope hook on a pool configuration.
func Configure(config *pgxpool.Config) *pgxpool.Config {
	next := config.PrepareConn
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		s, _ := ctx.Value(scopeKey{}).(scope)
		system := "off"
		if s.system {
			system = "on"
		}
		if _, err := conn.Exec(ctx, `SELECT set_config('blaxsmith.org_id', $1, false), set_config('blaxsmith.system', $2, false)`, s.org, system); err != nil {
			return false, fmt.Errorf("scope database connection: %w", err)
		}
		if next != nil {
			return next(ctx, conn)
		}
		return true, nil
	}
	return config
}

// NewPool opens a pool whose connections carry the acquiring context's scope.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	return pgxpool.NewWithConfig(ctx, Configure(config))
}
