package access

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrStale = errors.New("secret version changed")

const maxSecretBytes = 16 << 10

// SecretStore holds the master keys supplied by trusted platform
// configuration. Key bytes must be backed up separately from PostgreSQL.
//
// Secrets use envelope encryption: each organization has its own random data
// key (access_organization_keys), stored only wrapped by a master key. A
// secret row is sealed by its organization's data key, so one organization's
// key opens nothing of another's, and rotating the master key re-wraps data
// keys without touching secret ciphertext. Rows written before 0150 are sealed
// directly by a master key (data_key_version IS NULL); they stay readable and
// are re-encrypted under the data key on the next write for their connection
// or by UpgradeLegacy. Unwrapped data keys are never cached: the wrapped key
// arrives in the same query as the ciphertext and is cleared after use.
type SecretStore struct {
	db      *pgxpool.Pool
	current string
	keys    map[string][]byte
}

type Secret struct {
	Version   int64
	KeyID     string
	Bytes     []byte
	ExpiresAt *time.Time
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
	ctx = tenant.Org(ctx, organizationID)
	if s == nil || organizationID == "" || connectionID == "" || expectedVersion < 0 ||
		len(plaintext) == 0 || len(plaintext) > maxSecretBytes {
		return 0, ErrDenied
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin secret rotation: %w", err)
	}
	defer tx.Rollback(ctx)
	version, err := s.RotateTx(ctx, tx, organizationID, connectionID, expectedVersion, plaintext, expiresAt)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit secret rotation: %w", err)
	}
	return version, nil
}

// RotateTx lets connection/grant onboarding and the encrypted secret commit
// together. The caller owns and commits the transaction.
func (s *SecretStore) RotateTx(ctx context.Context, tx pgx.Tx, organizationID, connectionID string,
	expectedVersion int64, plaintext []byte, expiresAt *time.Time) (int64, error) {
	if s == nil || tx == nil || organizationID == "" || connectionID == "" || expectedVersion < 0 ||
		len(plaintext) == 0 || len(plaintext) > maxSecretBytes {
		return 0, ErrDenied
	}
	var state string
	var previous *int64
	err := tx.QueryRow(ctx, `SELECT state, active_secret_version FROM access_connections
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
	if err := s.insertVersion(ctx, tx, organizationID, connectionID, version, plaintext, expiresAt); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE access_connections SET active_secret_version=$3
		WHERE organization_id=$1 AND id=$2`, organizationID, connectionID, version); err != nil {
		return 0, fmt.Errorf("activate secret version: %w", err)
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
	return s.readVersion(ctx, tx, organizationID, connectionID, *version)
}
