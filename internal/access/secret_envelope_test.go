package access

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func envelopeFixture(t *testing.T, pool *pgxpool.Pool, organizationID string, connections ...string) {
	t.Helper()
	ctx := tenant.System(context.Background())
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations
		(organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,'git','git','https://git.example.invalid',ARRAY['native_raw'],'active')`, organizationID); err != nil {
		t.Fatal(err)
	}
	for _, id := range connections {
		if _, err := pool.Exec(ctx, `INSERT INTO access_connections
			(organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
			VALUES ($1,$2,'user','alice','git',$2,'test','active')`, organizationID, id); err != nil {
			t.Fatal(err)
		}
	}
}

func readSecret(t *testing.T, pool *pgxpool.Pool, store *SecretStore, organizationID, connectionID string, version int64) (string, error) {
	t.Helper()
	ctx := tenant.System(context.Background())
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var secret Secret
	if version == 0 {
		secret, err = store.ReadCurrent(ctx, tx, organizationID, connectionID)
	} else {
		secret, err = store.readVersion(ctx, tx, organizationID, connectionID, version)
	}
	defer secret.Clear()
	return string(secret.Bytes), err
}

func organizationKey(t *testing.T, pool *pgxpool.Pool, store *SecretStore, organizationID string) (string, []byte) {
	t.Helper()
	var masterKeyID string
	var nonce, wrapped []byte
	if err := pool.QueryRow(tenant.System(context.Background()), `SELECT master_key_id, nonce, wrapped_key
		FROM access_organization_keys WHERE organization_id=$1 AND version=1`, organizationID).
		Scan(&masterKeyID, &nonce, &wrapped); err != nil {
		t.Fatal(err)
	}
	key, err := store.unwrap(organizationID, 1, masterKeyID, nonce, wrapped)
	if err != nil {
		t.Fatalf("unwrap %s key: %v", organizationID, err)
	}
	return masterKeyID, key
}

func TestSecretEnvelopeOrganizationSeparation(t *testing.T) {
	ctx := tenant.System(context.Background())
	pool := testPool(t)
	envelopeFixture(t, pool, "org-a", "conn")
	envelopeFixture(t, pool, "org-b", "conn")
	store, err := NewSecretStore(pool, "master", map[string][]byte{"master": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org-a", "org-b"} {
		if _, err := store.Rotate(ctx, org, "conn", 0, []byte("secret-of-"+org), nil); err != nil {
			t.Fatal(err)
		}
		if got, err := readSecret(t, pool, store, org, "conn", 0); err != nil || got != "secret-of-"+org {
			t.Fatalf("%s read: %q %v", org, got, err)
		}
	}
	var keys, legacy int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM access_organization_keys),
		(SELECT count(*) FROM access_secret_versions WHERE data_key_version IS NULL)`).Scan(&keys, &legacy); err != nil {
		t.Fatal(err)
	}
	if keys != 2 || legacy != 0 {
		t.Fatalf("organization keys=%d legacy rows=%d", keys, legacy)
	}
	_, keyA := organizationKey(t, pool, store, "org-a")
	_, keyB := organizationKey(t, pool, store, "org-b")
	defer clear(keyA)
	defer clear(keyB)
	if bytes.Equal(keyA, keyB) {
		t.Fatal("organizations share a data key")
	}
	var nonce, ciphertext []byte
	if err := pool.QueryRow(ctx, `SELECT nonce, ciphertext FROM access_secret_versions
		WHERE organization_id='org-b' AND connection_id='conn' AND version=1`).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := open(keyA, nonce, ciphertext, dataSecretAAD("org-b", "conn", 1, 1)); err == nil {
		t.Fatal("org-a data key opened org-b secret")
	}
	if data, err := open(keyB, nonce, ciphertext, dataSecretAAD("org-b", "conn", 1, 1)); err != nil || string(data) != "secret-of-org-b" {
		t.Fatalf("org-b data key: %v", err)
	}

	// Tamper: org-b's row pointing at org-a's wrapped key fails its AAD.
	if _, err := pool.Exec(ctx, `UPDATE access_organization_keys b SET nonce=a.nonce, wrapped_key=a.wrapped_key
		FROM access_organization_keys a WHERE a.organization_id='org-a' AND b.organization_id='org-b'`); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(t, pool, store, "org-b", "conn", 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("swapped wrapped key: %v", err)
	}
	if _, err := store.Rotate(ctx, "org-b", "conn", 1, []byte("next"), nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("write with swapped wrapped key: %v", err)
	}
	// Tamper: a relabeled master key ID no longer matches the wrap AAD.
	if _, err := pool.Exec(ctx, `UPDATE access_organization_keys SET master_key_id='other'
		WHERE organization_id='org-a'`); err != nil {
		t.Fatal(err)
	}
	other, err := NewSecretStore(pool, "master", map[string][]byte{
		"master": bytes.Repeat([]byte{7}, 32), "other": bytes.Repeat([]byte{7}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(t, pool, other, "org-a", "conn", 0); !errors.Is(err, ErrDenied) {
		t.Fatalf("relabeled master key: %v", err)
	}
}

func TestSecretEnvelopeLegacyUpgrade(t *testing.T) {
	ctx := tenant.System(context.Background())
	pool := testPool(t)
	envelopeFixture(t, pool, "org-a", "lazy", "batch-1", "batch-2", "orphan")
	master := bytes.Repeat([]byte{3}, 32)
	store, err := NewSecretStore(pool, "primary", map[string][]byte{"primary": master})
	if err != nil {
		t.Fatal(err)
	}
	// Rows as written before 0150: sealed directly by the master key.
	legacy := func(connectionID string, version int64, keyID string, key []byte, plaintext string) {
		t.Helper()
		nonce, ciphertext, err := seal(key, []byte(plaintext), secretAAD("org-a", connectionID, version, keyID))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO access_secret_versions
			(organization_id,connection_id,version,key_id,algorithm,nonce,ciphertext)
			VALUES ('org-a',$1,$2,$3,'AES-256-GCM',$4,$5)`,
			connectionID, version, keyID, nonce, ciphertext); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE access_connections SET active_secret_version=$2
			WHERE organization_id='org-a' AND id=$1`, connectionID, version); err != nil {
			t.Fatal(err)
		}
	}
	legacy("lazy", 1, "primary", master, "lazy-v1")
	legacy("batch-1", 1, "primary", master, "batch-1-v1")
	legacy("batch-1", 2, "primary", master, "batch-1-v2")
	legacy("batch-2", 1, "primary", master, "batch-2-v1")
	legacy("orphan", 1, "retired", bytes.Repeat([]byte{4}, 32), "orphan-v1")

	if got, err := readSecret(t, pool, store, "org-a", "lazy", 0); err != nil || got != "lazy-v1" {
		t.Fatalf("legacy read: %q %v", got, err)
	}
	// Writing a new version lazily re-encrypts that connection's legacy rows.
	if _, err := store.Rotate(ctx, "org-a", "lazy", 1, []byte("lazy-v2"), nil); err != nil {
		t.Fatal(err)
	}
	var upgraded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_secret_versions
		WHERE connection_id='lazy' AND data_key_version=1 AND key_id='org-dek-1'`).Scan(&upgraded); err != nil {
		t.Fatal(err)
	}
	if upgraded != 2 {
		t.Fatalf("lazy upgrade left legacy rows: %d upgraded", upgraded)
	}
	for version, want := range map[int64]string{1: "lazy-v1", 2: "lazy-v2"} {
		if got, err := readSecret(t, pool, store, "org-a", "lazy", version); err != nil || got != want {
			t.Fatalf("lazy v%d: %q %v", version, got, err)
		}
	}
	var others int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_secret_versions
		WHERE connection_id<>'lazy' AND data_key_version IS NOT NULL`).Scan(&others); err != nil {
		t.Fatal(err)
	}
	if others != 0 {
		t.Fatal("lazy upgrade touched other connections")
	}

	// The batch job upgrades the rest; a batch of one exercises paging. The
	// row whose master key is absent stays legacy and is reported.
	for run := 0; run < 2; run++ {
		remaining, err := store.UpgradeLegacy(ctx, 1)
		if err != nil || remaining != 1 {
			t.Fatalf("upgrade run %d: remaining=%d err=%v", run, remaining, err)
		}
	}
	for _, c := range []struct {
		connection string
		version    int64
		want       string
	}{{"batch-1", 1, "batch-1-v1"}, {"batch-1", 2, "batch-1-v2"}, {"batch-2", 1, "batch-2-v1"}} {
		if got, err := readSecret(t, pool, store, "org-a", c.connection, c.version); err != nil || got != c.want {
			t.Fatalf("%s v%d after upgrade: %q %v", c.connection, c.version, got, err)
		}
	}
	var orphanKeyID string
	if err := pool.QueryRow(ctx, `SELECT key_id FROM access_secret_versions
		WHERE connection_id='orphan' AND data_key_version IS NULL`).Scan(&orphanKeyID); err != nil || orphanKeyID != "retired" {
		t.Fatalf("orphan row: %q %v", orphanKeyID, err)
	}
	// An upgraded row no longer opens with the master key alone.
	var nonce, ciphertext []byte
	if err := pool.QueryRow(ctx, `SELECT nonce, ciphertext FROM access_secret_versions
		WHERE connection_id='batch-2' AND version=1`).Scan(&nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if _, err := open(master, nonce, ciphertext, secretAAD("org-a", "batch-2", 1, "primary")); err == nil {
		t.Fatal("upgraded row still sealed by the master key")
	}
	// Tamper: flipping an upgraded row back to legacy does not downgrade it.
	if _, err := pool.Exec(ctx, `UPDATE access_secret_versions SET data_key_version=NULL, key_id='primary'
		WHERE connection_id='batch-2' AND version=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := readSecret(t, pool, store, "org-a", "batch-2", 1); !errors.Is(err, ErrDenied) {
		t.Fatalf("relabeled row: %v", err)
	}
}

func TestSecretEnvelopeMasterKeyRotation(t *testing.T) {
	ctx := tenant.System(context.Background())
	pool := testPool(t)
	envelopeFixture(t, pool, "org-a", "conn")
	envelopeFixture(t, pool, "org-b", "conn")
	oldKey, newKey := bytes.Repeat([]byte{5}, 32), bytes.Repeat([]byte{6}, 32)
	before, err := NewSecretStore(pool, "old", map[string][]byte{"old": oldKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org-a", "org-b"} {
		if _, err := before.Rotate(ctx, org, "conn", 0, []byte("secret-of-"+org), nil); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() string {
		t.Helper()
		var digest string
		if err := pool.QueryRow(ctx, `SELECT string_agg(encode(nonce||ciphertext,'hex'),',' ORDER BY organization_id)
			FROM access_secret_versions`).Scan(&digest); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	ciphertexts := snapshot()
	during, err := NewSecretStore(pool, "new", map[string][]byte{"old": oldKey, "new": newKey})
	if err != nil {
		t.Fatal(err)
	}
	remaining, err := during.RewrapKeys(ctx)
	if err != nil || remaining != 0 {
		t.Fatalf("rewrap: remaining=%d err=%v", remaining, err)
	}
	if remaining, err := during.RewrapKeys(ctx); err != nil || remaining != 0 {
		t.Fatalf("second rewrap: remaining=%d err=%v", remaining, err)
	}
	if snapshot() != ciphertexts {
		t.Fatal("master key rotation re-encrypted secret rows")
	}
	var underNew int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM access_organization_keys
		WHERE master_key_id='new' AND rewrapped_at IS NOT NULL`).Scan(&underNew); err != nil || underNew != 2 {
		t.Fatalf("rewrapped keys: %d %v", underNew, err)
	}
	after, err := NewSecretStore(pool, "new", map[string][]byte{"new": newKey})
	if err != nil {
		t.Fatal(err)
	}
	for _, org := range []string{"org-a", "org-b"} {
		if got, err := readSecret(t, pool, after, org, "conn", 0); err != nil || got != "secret-of-"+org {
			t.Fatalf("%s after rotation: %q %v", org, got, err)
		}
		if _, err := readSecret(t, pool, before, org, "conn", 0); !errors.Is(err, ErrDenied) {
			t.Fatalf("%s opened with the retired master key: %v", org, err)
		}
	}
	if _, err := after.Rotate(ctx, "org-a", "conn", 1, []byte("post-rotation"), nil); err != nil {
		t.Fatal(err)
	}
	if got, err := readSecret(t, pool, after, "org-a", "conn", 0); err != nil || got != "post-rotation" {
		t.Fatalf("write after rotation: %q %v", got, err)
	}
}
