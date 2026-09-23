package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
)

func TestPassword(t *testing.T) {
	password := []byte("correct horse battery staple")
	hash, err := HashPassword(password)
	if err != nil || !strings.HasPrefix(hash, hashPrefix) || !VerifyPassword(hash, password) ||
		VerifyPassword(hash, []byte("wrong horse battery staple")) || VerifyPassword(hash+"!", password) {
		t.Fatalf("password hash/verification failed: %v", err)
	}
	if _, err := HashPassword([]byte("too short")); !errors.Is(err, ErrPassword) {
		t.Fatalf("short password accepted: %v", err)
	}
}

func TestBootstrapOwnerPostgres(t *testing.T) {
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
	schema := "blaxsmith_identity_" + hex.EncodeToString(suffix[:])
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
	t.Cleanup(pool.Close)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	password := []byte("correct horse battery staple")
	var wg sync.WaitGroup
	type result struct {
		owner FirstOwner
		err   error
	}
	results := make(chan result, 2)
	for _, candidate := range []struct{ username, slug string }{{"alice", "team-a"}, {"bob", "team-b"}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner, err := BootstrapOwner(ctx, pool, candidate.username, candidate.slug, "Engineering", password)
			results <- result{owner, err}
		}()
	}
	wg.Wait()
	close(results)
	success, denied := 0, 0
	for result := range results {
		if result.err == nil && result.owner.PrincipalID != "" && result.owner.OrganizationID != "" {
			success++
		} else if errors.Is(result.err, ErrBootstrapped) {
			denied++
		} else {
			t.Fatalf("unexpected bootstrap result: %+v", result)
		}
	}
	if success != 1 || denied != 1 {
		t.Fatalf("first-owner race: %d succeeded, %d denied", success, denied)
	}
	var principals, orgs, memberships, installations, audits int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM identity_principals),
		(SELECT count(*) FROM identity_organizations),
		(SELECT count(*) FROM identity_memberships),
		(SELECT count(*) FROM identity_installation),
		(SELECT count(*) FROM identity_audit_events)`).
		Scan(&principals, &orgs, &memberships, &installations, &audits); err != nil {
		t.Fatal(err)
	}
	if principals != 1 || orgs != 1 || memberships != 1 || installations != 1 || audits != 1 {
		t.Fatalf("partial bootstrap: %d/%d/%d/%d/%d", principals, orgs, memberships, installations, audits)
	}
	var hash string
	if err := pool.QueryRow(ctx, `SELECT password_hash FROM identity_principals`).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash == string(password) || !VerifyPassword(hash, password) {
		t.Fatal("owner password was not stored as an Argon2id hash")
	}
}
