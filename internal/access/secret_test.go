package access

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestSecretStorePostgres(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ('org-a','git','git','https://git.example.invalid',ARRAY['native_raw'],'active');
		INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ('org-a','connection-a','user','alice','git','account-a','test','active')`); err != nil {
		t.Fatal(err)
	}
	key1, key2 := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	store1, err := NewSecretStore(pool, "key-1", map[string][]byte{"key-1": key1})
	if err != nil {
		t.Fatal(err)
	}
	clear(key1) // The store owns an independent key copy.
	const marker = "synthetic-secret-never-in-metadata"
	version, err := store1.Rotate(ctx, "org-a", "connection-a", 0, []byte(marker), nil)
	if err != nil || version != 1 {
		t.Fatalf("first secret version: %d, %v", version, err)
	}
	var nonce, ciphertext []byte
	if err := pool.QueryRow(ctx, `SELECT nonce, ciphertext FROM access_secret_versions
		WHERE organization_id='org-a' AND connection_id='connection-a' AND version=1`).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if len(nonce) != 12 || bytes.Contains(ciphertext, []byte(marker)) || bytes.Equal(ciphertext, []byte(marker)) {
		t.Fatal("plaintext persisted instead of authenticated ciphertext")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_connections
		(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,active_secret_version)
		VALUES ('org-a','connection-b','user','bob','git','account-b','test','active',1);
		INSERT INTO access_secret_versions
		(organization_id,connection_id,version,key_id,algorithm,nonce,ciphertext)
		SELECT organization_id,'connection-b',version,key_id,algorithm,nonce,ciphertext
		FROM access_secret_versions WHERE organization_id='org-a' AND connection_id='connection-a' AND version=1`); err != nil {
		t.Fatal(err)
	}
	copyTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, copiedErr := store1.ReadCurrent(ctx, copyTx, "org-a", "connection-b")
	_ = copyTx.Rollback(ctx)
	if !errors.Is(copiedErr, ErrDenied) {
		t.Fatalf("copied ciphertext opened for a different connection: %v", copiedErr)
	}
	read := func(store *SecretStore, organizationID string) (Secret, error) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		return store.ReadCurrent(ctx, tx, organizationID, "connection-a")
	}
	secret, err := read(store1, "org-a")
	if err != nil || secret.Version != 1 || string(secret.Bytes) != marker {
		t.Fatalf("current secret: version=%d err=%v", secret.Version, err)
	}
	secret.Clear()
	if !bytes.Equal(secret.Bytes, make([]byte, len(marker))) {
		t.Fatal("returned secret was not cleared")
	}
	if _, err := read(store1, "org-b"); !errors.Is(err, ErrDenied) {
		t.Fatalf("cross-organization secret read: %v", err)
	}
	wrongKeyStore, err := NewSecretStore(pool, "key-2", map[string][]byte{"key-2": key2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := read(wrongKeyStore, "org-a"); !errors.Is(err, ErrDenied) {
		t.Fatalf("missing old key read: %v", err)
	}
	store2, err := NewSecretStore(pool, "key-2", map[string][]byte{"key-1": bytes.Repeat([]byte{1}, 32), "key-2": key2})
	if err != nil {
		t.Fatal(err)
	}
	old, err := read(store2, "org-a")
	if err != nil || string(old.Bytes) != marker {
		t.Fatalf("old key during key rotation: %v", err)
	}
	old.Clear()
	version, err = store2.Rotate(ctx, "org-a", "connection-a", 1, []byte("second-synthetic-secret"), nil)
	if err != nil || version != 2 {
		t.Fatalf("rotate encryption key: %d, %v", version, err)
	}
	if _, err := store1.Rotate(ctx, "org-a", "connection-a", 1, []byte("stale-refresh"), nil); !errors.Is(err, ErrStale) {
		t.Fatalf("stale refresh advanced version: %v", err)
	}
	if _, err := read(store1, "org-a"); !errors.Is(err, ErrDenied) {
		t.Fatalf("old key read new secret: %v", err)
	}
	current, err := read(store2, "org-a")
	if err != nil || current.Version != 2 || current.KeyID != "org-dek-1" || string(current.Bytes) != "second-synthetic-secret" {
		t.Fatalf("new key/current version: %+v, %v", current, err)
	}
	current.Clear()

	// A release reading this version holds the connection row until send ends.
	locked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reading, err := store2.ReadCurrent(ctx, locked, "org-a", "connection-a")
	if err != nil {
		t.Fatal(err)
	}
	reading.Clear()
	rotateCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	_, err = store2.Rotate(rotateCtx, "org-a", "connection-a", 2, []byte("blocked-rotation"), nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("rotation did not wait for credential-read lock: %v", err)
	}
	if err := locked.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, value := range []string{"third-a", "third-b"} {
		group.Go(func() {
			_, err := store2.Rotate(ctx, "org-a", "connection-a", 2, []byte(value), nil)
			results <- err
		})
	}
	group.Wait()
	close(results)
	var successes, stale int
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrStale):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if successes != 1 || stale != 1 {
		t.Fatalf("concurrent refresh: success=%d stale=%d", successes, stale)
	}
	if _, err := pool.Exec(ctx, `UPDATE access_secret_versions SET expires_at=clock_timestamp()-interval '1 second'
		WHERE organization_id='org-a' AND connection_id='connection-a' AND version=3`); err != nil {
		t.Fatal(err)
	}
	if _, err := read(store2, "org-a"); !errors.Is(err, ErrDenied) {
		t.Fatalf("expired secret read: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE access_secret_versions SET expires_at=NULL,
		ciphertext=decode(repeat('00',octet_length(ciphertext)),'hex')
		WHERE organization_id='org-a' AND connection_id='connection-a' AND version=3`); err != nil {
		t.Fatal(err)
	}
	if _, err := read(store2, "org-a"); !errors.Is(err, ErrDenied) {
		t.Fatalf("tampered ciphertext read: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE access_connections SET state='disabled'
		WHERE organization_id='org-a' AND id='connection-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store2.Rotate(ctx, "org-a", "connection-a", 3, []byte("disallowed"), nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled connection rotated: %v", err)
	}
	if _, err := read(store2, "org-a"); !errors.Is(err, ErrDenied) {
		t.Fatalf("disabled connection read: %v", err)
	}
}
