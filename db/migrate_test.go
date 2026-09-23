package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMigratePostgres(t *testing.T) {
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_migrate_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanupCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := Verify(ctx, pool); err == nil {
		t.Fatal("verify accepted a database without a migration ledger")
	}
	var wg sync.WaitGroup
	counts := make(chan int, 2)
	errors := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			count, err := Migrate(ctx, pool)
			counts <- count
			errors <- err
		}()
	}
	wg.Wait()
	close(counts)
	close(errors)
	total := 0
	for count := range counts {
		total += count
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil || total != len(names) {
		t.Fatalf("concurrent migrations applied %d versions, want %d: %v", total, len(names), err)
	}
	if count, err := Migrate(ctx, pool); err != nil || count != 0 {
		t.Fatalf("repeat migration: %d, %v", count, err)
	}
	if err := Verify(ctx, pool); err != nil {
		t.Fatalf("verify migrated database: %v", err)
	}
	var firstDigest string
	if err := pool.QueryRow(ctx, `SELECT sha256 FROM blaxsmith_schema_migrations WHERE version='0001_bootstrap'`).Scan(&firstDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM blaxsmith_schema_migrations WHERE version='0001_bootstrap'`); err != nil {
		t.Fatal(err)
	}
	if err := Verify(ctx, pool); err == nil || !strings.Contains(err.Error(), "is not applied") {
		t.Fatalf("verify accepted missing migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blaxsmith_schema_migrations(version,sha256) VALUES ('0001_bootstrap',$1)`, firstDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE blaxsmith_schema_migrations SET sha256='changed' WHERE version='0001_bootstrap'`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("edited migration was accepted: %v", err)
	}
	if err := Verify(ctx, pool); err == nil || !strings.Contains(err.Error(), "checksum changed") {
		t.Fatalf("verify accepted edited migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blaxsmith_schema_migrations (version,sha256) VALUES ('9999_future','future')`); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(ctx, pool); err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
		t.Fatalf("unknown future migration was accepted: %v", err)
	}
}
