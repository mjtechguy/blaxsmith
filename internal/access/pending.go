package access

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Pending sign-ins (OAuth state with its PKCE verifier, device codes) live in
// PostgreSQL so any replica can finish a flow another one started. The row is
// keyed by a hash of the browser-held handle, bound to the organization,
// principal, and session that started it, and its payload is encrypted with
// the secret store's current key.
var (
	ErrPendingBusy    = errors.New("another request is checking this sign-in")
	ErrTooManyPending = errors.New("too many pending sign-ins")
)

const maxPendingPerPrincipal = 20

// PendingOwner is the session a pending sign-in is bound to.
type PendingOwner struct{ OrganizationID, PrincipalID, SessionID string }

func pendingKey(kind, handle string) []byte {
	hash := sha256.Sum256([]byte(kind + "\x00" + handle))
	return hash[:]
}

func pendingAAD(kind string, key []byte, owner PendingOwner) []byte {
	data, _ := json.Marshal([]string{"pending", kind, hex.EncodeToString(key), owner.OrganizationID, owner.PrincipalID, owner.SessionID})
	return data
}

// SavePending stores payload (which may be secret) until expires, and
// opportunistically deletes every expired pending sign-in.
func (s *SecretStore) SavePending(ctx context.Context, kind, handle string, owner PendingOwner, payload []byte, expires time.Time) error {
	ctx = tenant.Org(ctx, owner.OrganizationID)
	if s == nil || kind == "" || handle == "" || owner.OrganizationID == "" || owner.PrincipalID == "" ||
		owner.SessionID == "" || len(payload) == 0 || len(payload) > maxSecretBytes {
		return ErrDenied
	}
	key := pendingKey(kind, handle)
	nonce, ciphertext, err := seal(s.keys[s.current], payload, pendingAAD(kind, key, owner))
	if err != nil {
		return err
	}
	// The sweep spans organizations; the insert and its per-principal cap are
	// scoped to the owner's organization.
	if _, err := s.db.Exec(tenant.System(ctx), `DELETE FROM access_pending_sign_ins WHERE expires_at<clock_timestamp()`); err != nil {
		return fmt.Errorf("sweep pending sign-ins: %w", err)
	}
	tag, err := s.db.Exec(ctx, `INSERT INTO access_pending_sign_ins
		(key_hash,kind,organization_id,principal_id,session_id,key_id,nonce,ciphertext,expires_at)
		SELECT $1,$2,$3,$4,$5,$6,$7,$8,$9
		WHERE (SELECT count(*) FROM access_pending_sign_ins WHERE principal_id=$4) < $10`,
		key, kind, owner.OrganizationID, owner.PrincipalID, owner.SessionID, s.current, nonce, ciphertext, expires,
		maxPendingPerPrincipal)
	if err != nil {
		return fmt.Errorf("save pending sign-in: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrTooManyPending
	}
	return nil
}

func (s *SecretStore) openPending(kind string, key []byte, owner PendingOwner, keyID string, nonce, ciphertext []byte) ([]byte, error) {
	master, ok := s.keys[keyID]
	if !ok || len(ciphertext) > maxSecretBytes+16 {
		return nil, ErrDenied
	}
	data, err := open(master, nonce, ciphertext, pendingAAD(kind, key, owner))
	if err != nil || len(data) == 0 || len(data) > maxSecretBytes {
		return nil, ErrDenied
	}
	return data, nil
}

// TakePending consumes a pending sign-in exactly once, whoever presents the
// handle; the caller must compare the returned owner with its own session.
// A missing or expired handle is ErrDenied.
func (s *SecretStore) TakePending(ctx context.Context, kind, handle string) (PendingOwner, []byte, error) {
	ctx = tenant.System(ctx) // the handle hash is the credential; the caller checks the returned owner
	if s == nil || kind == "" || handle == "" {
		return PendingOwner{}, nil, ErrDenied
	}
	key := pendingKey(kind, handle)
	var owner PendingOwner
	var keyID string
	var nonce, ciphertext []byte
	var live bool
	err := s.db.QueryRow(ctx, `DELETE FROM access_pending_sign_ins WHERE key_hash=$1 AND kind=$2
		RETURNING organization_id,principal_id,session_id,key_id,nonce,ciphertext,expires_at>clock_timestamp()`,
		key, kind).Scan(&owner.OrganizationID, &owner.PrincipalID, &owner.SessionID, &keyID, &nonce, &ciphertext, &live)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !live) {
		return PendingOwner{}, nil, ErrDenied
	}
	if err != nil {
		return PendingOwner{}, nil, fmt.Errorf("take pending sign-in: %w", err)
	}
	payload, err := s.openPending(kind, key, owner, keyID, nonce, ciphertext)
	return owner, payload, err
}

// ClaimPending gives owner's session the pending sign-in for up to hold
// without consuming it, so one replica at a time polls the provider. It
// returns ErrPendingBusy while another claim holds it and ErrDenied when it
// is gone, expired, or bound to another session.
func (s *SecretStore) ClaimPending(ctx context.Context, kind, handle string, owner PendingOwner, hold time.Duration) ([]byte, error) {
	ctx = tenant.Org(ctx, owner.OrganizationID)
	if s == nil || kind == "" || handle == "" || owner.SessionID == "" || owner.PrincipalID == "" {
		return nil, ErrDenied
	}
	key := pendingKey(kind, handle)
	var keyID string
	var nonce, ciphertext []byte
	err := s.db.QueryRow(ctx, `UPDATE access_pending_sign_ins SET claimed_until=clock_timestamp()+$6::interval
		WHERE key_hash=$1 AND kind=$2 AND organization_id=$3 AND principal_id=$4 AND session_id=$5
		AND expires_at>clock_timestamp() AND (claimed_until IS NULL OR claimed_until<clock_timestamp())
		RETURNING key_id,nonce,ciphertext`, key, kind, owner.OrganizationID, owner.PrincipalID, owner.SessionID, hold).
		Scan(&keyID, &nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		var held bool
		err = s.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM access_pending_sign_ins
			WHERE key_hash=$1 AND kind=$2 AND organization_id=$3 AND principal_id=$4 AND session_id=$5
			AND expires_at>clock_timestamp())`, key, kind, owner.OrganizationID, owner.PrincipalID, owner.SessionID).Scan(&held)
		if err == nil && held {
			return nil, ErrPendingBusy
		}
		if err == nil {
			return nil, ErrDenied
		}
	}
	if err != nil {
		return nil, fmt.Errorf("claim pending sign-in: %w", err)
	}
	return s.openPending(kind, key, owner, keyID, nonce, ciphertext)
}

// FinishPending ends a claim: done deletes the pending sign-in, otherwise it
// is released for the next poll.
func (s *SecretStore) FinishPending(ctx context.Context, kind, handle string, done bool) error {
	ctx = tenant.System(ctx) // the handle hash is the credential; the caller checks the returned owner
	if s == nil {
		return ErrDenied
	}
	query := `UPDATE access_pending_sign_ins SET claimed_until=NULL WHERE key_hash=$1 AND kind=$2`
	if done {
		query = `DELETE FROM access_pending_sign_ins WHERE key_hash=$1 AND kind=$2`
	}
	_, err := s.db.Exec(ctx, query, pendingKey(kind, handle), kind)
	return err
}
