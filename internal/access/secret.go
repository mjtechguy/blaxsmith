package access

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrStale = errors.New("secret version changed")

const maxSecretBytes = 16 << 10

// SecretStore holds database encryption keys supplied by trusted platform
// configuration. Key bytes must be backed up separately from PostgreSQL.
type SecretStore struct {
	db      *pgxpool.Pool
	current string
	keys    map[string][]byte
}

type Secret struct {
	Version int64
	KeyID   string
	Bytes   []byte
}

func (s *Secret) Clear() { clear(s.Bytes) }

func NewSecretStore(db *pgxpool.Pool, current string, keys map[string][]byte) (*SecretStore, error) {
	if db == nil || current == "" || len(keys[current]) != 32 {
		return nil, ErrDenied
	}
	copyKeys := make(map[string][]byte, len(keys))
	for id, key := range keys {
		if id == "" || len(key) != 32 {
			return nil, ErrDenied
		}
		copyKeys[id] = append([]byte(nil), key...)
	}
	return &SecretStore{db: db, current: current, keys: copyKeys}, nil
}

// Rotate inserts a new encrypted version and advances the connection pointer
// only if the caller still owns the previously observed version. Zero means a
// connection with no secret yet. The caller retains and must clear plaintext.
func (s *SecretStore) Rotate(ctx context.Context, organizationID, connectionID string,
	expectedVersion int64, plaintext []byte, expiresAt *time.Time) (int64, error) {
	if s == nil || organizationID == "" || connectionID == "" || expectedVersion < 0 ||
		len(plaintext) == 0 || len(plaintext) > maxSecretBytes {
		return 0, ErrDenied
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin secret rotation: %w", err)
	}
	defer tx.Rollback(ctx)
	var state string
	var previous *int64
	err = tx.QueryRow(ctx, `SELECT state, active_secret_version FROM access_connections
		WHERE organization_id=$1 AND id=$2 FOR UPDATE`, organizationID, connectionID).
		Scan(&state, &previous)
	if err != nil {
		return 0, deniedOrError("connection", err)
	}
	current := int64(0)
	if previous != nil {
		current = *previous
	}
	if state != "active" || current == math.MaxInt64 {
		return 0, ErrDenied
	}
	if current != expectedVersion {
		return 0, ErrStale
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return 0, fmt.Errorf("read secret clock: %w", err)
	}
	if expiresAt != nil && !expiresAt.After(now) {
		return 0, ErrDenied
	}
	version := current + 1
	block, err := aes.NewCipher(s.keys[s.current])
	if err != nil {
		return 0, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return 0, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, err
	}
	ciphertext := aead.Seal(nil, nonce, plaintext, secretAAD(organizationID, connectionID, version, s.current))
	if _, err := tx.Exec(ctx, `INSERT INTO access_secret_versions
		(organization_id,connection_id,version,key_id,algorithm,nonce,ciphertext,expires_at)
		VALUES ($1,$2,$3,$4,'AES-256-GCM',$5,$6,$7)`,
		organizationID, connectionID, version, s.current, nonce, ciphertext, expiresAt); err != nil {
		return 0, fmt.Errorf("insert secret version: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE access_connections SET active_secret_version=$3
		WHERE organization_id=$1 AND id=$2`, organizationID, connectionID, version); err != nil {
		return 0, fmt.Errorf("activate secret version: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit secret rotation: %w", err)
	}
	return version, nil
}

// ReadCurrent must run in the same release transaction as the grant check.
// Its locks fence concurrent rotation or connection disable until delivery.
func (s *SecretStore) ReadCurrent(ctx context.Context, tx pgx.Tx, organizationID, connectionID string) (Secret, error) {
	if s == nil || tx == nil || organizationID == "" || connectionID == "" {
		return Secret{}, ErrDenied
	}
	var state string
	var version *int64
	err := tx.QueryRow(ctx, `SELECT state, active_secret_version FROM access_connections
		WHERE organization_id=$1 AND id=$2 FOR SHARE`, organizationID, connectionID).
		Scan(&state, &version)
	if err != nil {
		return Secret{}, deniedOrError("connection", err)
	}
	if state != "active" || version == nil {
		return Secret{}, ErrDenied
	}
	var keyID string
	var nonce, ciphertext []byte
	var expiresAt *time.Time
	err = tx.QueryRow(ctx, `SELECT key_id, nonce, ciphertext, expires_at FROM access_secret_versions
		WHERE organization_id=$1 AND connection_id=$2 AND version=$3 FOR SHARE`,
		organizationID, connectionID, *version).Scan(&keyID, &nonce, &ciphertext, &expiresAt)
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
	key, ok := s.keys[keyID]
	if !ok || len(nonce) != 12 || len(ciphertext) < 17 || len(ciphertext) > maxSecretBytes+16 {
		return Secret{}, ErrDenied
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Secret{}, ErrDenied
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Secret{}, ErrDenied
	}
	data, err := aead.Open(nil, nonce, ciphertext, secretAAD(organizationID, connectionID, *version, keyID))
	if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
		return Secret{}, ErrDenied
	}
	return Secret{Version: *version, KeyID: keyID, Bytes: data}, nil
}

func secretAAD(organizationID, connectionID string, version int64, keyID string) []byte {
	data, _ := json.Marshal([]string{organizationID, connectionID, strconv.FormatInt(version, 10), keyID})
	return data
}
