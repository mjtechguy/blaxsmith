package access

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// insertVersion encrypts one new version without moving the connection's
// active pointer. OAuth refresh uses it under its own session lock. It also
// lazily re-encrypts the connection's legacy master-key rows.
func (s *SecretStore) insertVersion(ctx context.Context, tx pgx.Tx, organizationID, connectionID string,
	version int64, plaintext []byte, expiresAt *time.Time) error {
	if len(plaintext) == 0 || len(plaintext) > maxSecretBytes {
		return ErrDenied
	}
	keyVersion, dataKey, err := s.writeKey(ctx, tx, organizationID)
	if err != nil {
		return err
	}
	defer clear(dataKey)
	nonce, ciphertext, err := seal(dataKey, plaintext, dataSecretAAD(organizationID, connectionID, version, keyVersion))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_secret_versions
		(organization_id,connection_id,version,key_id,algorithm,nonce,ciphertext,expires_at,data_key_version)
		VALUES ($1,$2,$3,$4,'AES-256-GCM',$5,$6,$7,$8)`,
		organizationID, connectionID, version, dataKeyID(keyVersion), nonce, ciphertext, expiresAt, keyVersion); err != nil {
		return fmt.Errorf("insert secret version: %w", err)
	}
	_, err = s.upgradeLegacy(ctx, tx, organizationID, connectionID, legacyCursor{}, 100, keyVersion, dataKey)
	return err
}

func (s *SecretStore) readVersion(ctx context.Context, tx pgx.Tx, organizationID, connectionID string, version int64) (Secret, error) {
	var keyID string
	var nonce, ciphertext []byte
	var expiresAt *time.Time
	var keyVersion *int32
	var masterKeyID *string
	var keyNonce, wrapped []byte
	err := tx.QueryRow(ctx, `SELECT s.key_id, s.nonce, s.ciphertext, s.expires_at, s.data_key_version,
		k.master_key_id, k.nonce, k.wrapped_key
		FROM access_secret_versions s LEFT JOIN access_organization_keys k
		ON k.organization_id=s.organization_id AND k.version=s.data_key_version
		WHERE s.organization_id=$1 AND s.connection_id=$2 AND s.version=$3 FOR SHARE OF s`,
		organizationID, connectionID, version).Scan(&keyID, &nonce, &ciphertext, &expiresAt,
		&keyVersion, &masterKeyID, &keyNonce, &wrapped)
	if err != nil {
		return Secret{}, deniedOrError("secret version", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return Secret{}, fmt.Errorf("read secret clock: %w", err)
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return Secret{}, ErrDenied
	}
	if len(ciphertext) > maxSecretBytes+16 {
		return Secret{}, ErrDenied
	}
	var data []byte
	if keyVersion == nil {
		key, ok := s.keys[keyID]
		if !ok {
			return Secret{}, ErrDenied
		}
		data, err = open(key, nonce, ciphertext, secretAAD(organizationID, connectionID, version, keyID))
	} else {
		if masterKeyID == nil {
			return Secret{}, ErrDenied
		}
		dataKey, unwrapErr := s.unwrap(organizationID, *keyVersion, *masterKeyID, keyNonce, wrapped)
		if unwrapErr != nil {
			return Secret{}, ErrDenied
		}
		data, err = open(dataKey, nonce, ciphertext, dataSecretAAD(organizationID, connectionID, version, *keyVersion))
		clear(dataKey)
	}
	if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
		clear(data)
		return Secret{}, ErrDenied
	}
	return Secret{Version: version, KeyID: keyID, Bytes: data, ExpiresAt: expiresAt}, nil
}

// writeKey returns the organization's active data key, creating it on the
// first write and re-wrapping it under the current master key if an older
// master key still wraps it. The caller clears the returned key.
func (s *SecretStore) writeKey(ctx context.Context, tx pgx.Tx, organizationID string) (int32, []byte, error) {
	for attempt := 0; attempt < 2; attempt++ {
		var version int32
		var masterKeyID string
		var nonce, wrapped []byte
		err := tx.QueryRow(ctx, `SELECT version, master_key_id, nonce, wrapped_key FROM access_organization_keys
			WHERE organization_id=$1 AND state='active'`, organizationID).Scan(&version, &masterKeyID, &nonce, &wrapped)
		if errors.Is(err, pgx.ErrNoRows) && attempt == 0 {
			if err := s.createKey(ctx, tx, organizationID); err != nil {
				return 0, nil, err
			}
			continue
		}
		if err != nil {
			return 0, nil, deniedOrError("organization key", err)
		}
		dataKey, err := s.unwrap(organizationID, version, masterKeyID, nonce, wrapped)
		if err != nil {
			return 0, nil, ErrDenied
		}
		if masterKeyID != s.current {
			if err := s.rewrapTx(ctx, tx, organizationID, version, masterKeyID, dataKey); err != nil {
				clear(dataKey)
				return 0, nil, err
			}
		}
		return version, dataKey, nil
	}
	return 0, nil, ErrDenied
}

// createKey inserts a fresh random data key. A concurrent first write for the
// same organization makes this a no-op; the caller re-reads the winner.
func (s *SecretStore) createKey(ctx context.Context, tx pgx.Tx, organizationID string) error {
	dataKey := make([]byte, 32)
	defer clear(dataKey)
	if _, err := rand.Read(dataKey); err != nil {
		return err
	}
	var version int32
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(version),0)+1 FROM access_organization_keys
		WHERE organization_id=$1`, organizationID).Scan(&version); err != nil {
		return fmt.Errorf("read organization key version: %w", err)
	}
	nonce, wrapped, err := seal(s.keys[s.current], dataKey, dataKeyAAD(organizationID, version, s.current))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_organization_keys
		(organization_id,version,master_key_id,algorithm,nonce,wrapped_key)
		VALUES ($1,$2,$3,'AES-256-GCM',$4,$5) ON CONFLICT DO NOTHING`,
		organizationID, version, s.current, nonce, wrapped); err != nil {
		return fmt.Errorf("insert organization key: %w", err)
	}
	return nil
}

func (s *SecretStore) rewrapTx(ctx context.Context, tx pgx.Tx, organizationID string, version int32, previous string, dataKey []byte) error {
	nonce, wrapped, err := seal(s.keys[s.current], dataKey, dataKeyAAD(organizationID, version, s.current))
	if err != nil {
		return err
	}
	// A concurrent re-wrap already moved it if master_key_id no longer matches.
	if _, err := tx.Exec(ctx, `UPDATE access_organization_keys SET master_key_id=$4, nonce=$5, wrapped_key=$6,
		rewrapped_at=clock_timestamp() WHERE organization_id=$1 AND version=$2 AND master_key_id=$3`,
		organizationID, version, previous, s.current, nonce, wrapped); err != nil {
		return fmt.Errorf("rewrap organization key: %w", err)
	}
	return nil
}

func (s *SecretStore) unwrap(organizationID string, version int32, masterKeyID string, nonce, wrapped []byte) ([]byte, error) {
	key, ok := s.keys[masterKeyID]
	if !ok {
		return nil, ErrDenied
	}
	dataKey, err := open(key, nonce, wrapped, dataKeyAAD(organizationID, version, masterKeyID))
	if err != nil || len(dataKey) != 32 {
		clear(dataKey)
		return nil, ErrDenied
	}
	return dataKey, nil
}

type legacyCursor struct {
	connectionID string
	version      int64
}

// upgradeLegacy re-encrypts up to limit of the organization's master-key rows
// (one connection's, when connectionID is set) under the given data key. Rows
// locked by a concurrent reader, sealed by an absent master key, or failing
// authentication are left as they are. It returns the last row examined, or
// the zero cursor when fewer than limit rows remained.
func (s *SecretStore) upgradeLegacy(ctx context.Context, tx pgx.Tx, organizationID, connectionID string,
	after legacyCursor, limit int, keyVersion int32, dataKey []byte) (legacyCursor, error) {
	keyIDs := make([]string, 0, len(s.keys))
	for id := range s.keys {
		keyIDs = append(keyIDs, id)
	}
	rows, err := tx.Query(ctx, `SELECT connection_id, version, key_id, nonce, ciphertext FROM access_secret_versions
		WHERE organization_id=$1 AND data_key_version IS NULL AND key_id=ANY($2)
		AND ($3='' OR connection_id=$3) AND (connection_id, version) > ($4, $5)
		ORDER BY connection_id, version LIMIT $6 FOR UPDATE SKIP LOCKED`,
		organizationID, keyIDs, connectionID, after.connectionID, after.version, limit)
	if err != nil {
		return legacyCursor{}, fmt.Errorf("list legacy secrets: %w", err)
	}
	type legacyRow struct {
		legacyCursor
		keyID             string
		nonce, ciphertext []byte
	}
	var legacy []legacyRow
	for rows.Next() {
		var row legacyRow
		if err := rows.Scan(&row.connectionID, &row.version, &row.keyID, &row.nonce, &row.ciphertext); err != nil {
			rows.Close()
			return legacyCursor{}, fmt.Errorf("read legacy secret: %w", err)
		}
		legacy = append(legacy, row)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return legacyCursor{}, fmt.Errorf("read legacy secrets: %w", err)
	}
	for _, row := range legacy {
		data, err := open(s.keys[row.keyID], row.nonce, row.ciphertext,
			secretAAD(organizationID, row.connectionID, row.version, row.keyID))
		if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
			clear(data)
			continue
		}
		nonce, ciphertext, err := seal(dataKey, data, dataSecretAAD(organizationID, row.connectionID, row.version, keyVersion))
		clear(data)
		if err != nil {
			return legacyCursor{}, err
		}
		if _, err := tx.Exec(ctx, `UPDATE access_secret_versions SET key_id=$4, nonce=$5, ciphertext=$6, data_key_version=$7
			WHERE organization_id=$1 AND connection_id=$2 AND version=$3 AND data_key_version IS NULL`,
			organizationID, row.connectionID, row.version, dataKeyID(keyVersion), nonce, ciphertext, keyVersion); err != nil {
			return legacyCursor{}, fmt.Errorf("upgrade legacy secret: %w", err)
		}
	}
	if len(legacy) < limit {
		return legacyCursor{}, nil
	}
	return legacy[len(legacy)-1].legacyCursor, nil
}

// UpgradeLegacy re-encrypts every legacy master-key secret under its
// organization's data key, in short per-organization batches. It is
// idempotent and safe beside live traffic; rows a reader holds are retried on
// the next run. It returns the number of legacy rows remaining afterwards.
func (s *SecretStore) UpgradeLegacy(ctx context.Context, batch int) (int, error) {
	if s == nil || batch <= 0 {
		return 0, ErrDenied
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT organization_id FROM access_secret_versions
		WHERE data_key_version IS NULL ORDER BY organization_id`)
	if err != nil {
		return 0, fmt.Errorf("list legacy secret organizations: %w", err)
	}
	// The listing above is the only cross-organization read; all work below
	// runs in transactions scoped to one organization.
	organizations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("list legacy secret organizations: %w", err)
	}
	for _, organizationID := range organizations {
		cursor := legacyCursor{}
		for {
			cursor, err = s.upgradeBatch(ctx, organizationID, cursor, batch)
			if err != nil {
				return 0, fmt.Errorf("upgrade secrets for %s: %w", organizationID, err)
			}
			if cursor == (legacyCursor{}) {
				break
			}
		}
	}
	var remaining int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM access_secret_versions
		WHERE data_key_version IS NULL`).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("count legacy secrets: %w", err)
	}
	return remaining, nil
}

func (s *SecretStore) upgradeBatch(ctx context.Context, organizationID string, after legacyCursor, batch int) (legacyCursor, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return legacyCursor{}, err
	}
	defer tx.Rollback(ctx)
	keyVersion, dataKey, err := s.writeKey(ctx, tx, organizationID)
	if err != nil {
		return legacyCursor{}, err
	}
	defer clear(dataKey)
	next, err := s.upgradeLegacy(ctx, tx, organizationID, "", after, batch, keyVersion, dataKey)
	if err != nil {
		return legacyCursor{}, err
	}
	return next, tx.Commit(ctx)
}

// RewrapKeys re-wraps every organization data key still sealed by a
// non-current master key, one organization per transaction. Secret rows are
// not touched. It returns the number of data keys still under another master
// key (those whose master key is not configured); at zero, older master keys
// no longer protect any data key.
func (s *SecretStore) RewrapKeys(ctx context.Context) (int, error) {
	if s == nil {
		return 0, ErrDenied
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT organization_id FROM access_organization_keys
		WHERE master_key_id<>$1 ORDER BY organization_id`, s.current)
	if err != nil {
		return 0, fmt.Errorf("list organization keys: %w", err)
	}
	organizations, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("list organization keys: %w", err)
	}
	for _, organizationID := range organizations {
		if err := s.rewrapOrganization(ctx, organizationID); err != nil {
			return 0, fmt.Errorf("rewrap keys for %s: %w", organizationID, err)
		}
	}
	var remaining int
	if err := s.db.QueryRow(ctx, `SELECT count(*) FROM access_organization_keys WHERE master_key_id<>$1`,
		s.current).Scan(&remaining); err != nil {
		return 0, fmt.Errorf("count organization keys: %w", err)
	}
	return remaining, nil
}

func (s *SecretStore) rewrapOrganization(ctx context.Context, organizationID string) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT version, master_key_id, nonce, wrapped_key FROM access_organization_keys
		WHERE organization_id=$1 AND master_key_id<>$2 FOR UPDATE`, organizationID, s.current)
	if err != nil {
		return err
	}
	type wrappedKey struct {
		version        int32
		masterKeyID    string
		nonce, wrapped []byte
	}
	var keys []wrappedKey
	for rows.Next() {
		var key wrappedKey
		if err := rows.Scan(&key.version, &key.masterKeyID, &key.nonce, &key.wrapped); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, key := range keys {
		dataKey, err := s.unwrap(organizationID, key.version, key.masterKeyID, key.nonce, key.wrapped)
		if err != nil {
			continue // Its master key is not configured here; it stays counted as remaining.
		}
		err = s.rewrapTx(ctx, tx, organizationID, key.version, key.masterKeyID, dataKey)
		clear(dataKey)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func seal(key, plaintext, aad []byte) (nonce, ciphertext []byte, err error) {
	aead, err := newGCM(key)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, aead.Seal(nil, nonce, plaintext, aad), nil
}

func open(key, nonce, ciphertext, aad []byte) ([]byte, error) {
	if len(nonce) != 12 || len(ciphertext) < 17 {
		return nil, ErrDenied
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, ErrDenied
	}
	return aead.Open(nil, nonce, ciphertext, aad)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// secretAAD binds a legacy row sealed directly by master key keyID.
func secretAAD(organizationID, connectionID string, version int64, keyID string) []byte {
	data, _ := json.Marshal([]string{organizationID, connectionID, strconv.FormatInt(version, 10), keyID})
	return data
}

// dataSecretAAD binds a row sealed by an organization data key. The scheme tag
// keeps it disjoint from secretAAD.
func dataSecretAAD(organizationID, connectionID string, version int64, keyVersion int32) []byte {
	data, _ := json.Marshal([]string{"secret/org-dek", organizationID, connectionID,
		strconv.FormatInt(version, 10), strconv.FormatInt(int64(keyVersion), 10)})
	return data
}

// dataKeyAAD binds a wrapped data key to its organization, version and the
// master key that wraps it.
func dataKeyAAD(organizationID string, version int32, masterKeyID string) []byte {
	data, _ := json.Marshal([]string{"org-dek", organizationID, strconv.FormatInt(int64(version), 10), masterKeyID})
	return data
}

func dataKeyID(version int32) string { return "org-dek-" + strconv.FormatInt(int64(version), 10) }
