package bootstrap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrDenied = errors.New("bootstrap authority denied")

type Scope struct {
	ClusterID       string
	AttemptID       string
	OwnerGeneration int64
}

type Actor struct {
	Atespace string
	Name     string
	UID      string
}

type Offer struct {
	ID            string
	Nonce         [32]byte
	Scope         Scope
	ActorAtespace string
	ActorName     string
	ActorUID      string
	ExpiresAt     time.Time
}

type Proof struct {
	Body           []byte
	CertificatePEM []byte
	Signature      []byte
}

type Redeemed struct {
	Scope         Scope
	ActorAtespace string
	ActorName     string
	ActorUID      string
	Challenge     Challenge
	ConsumedAt    time.Time
}

type Ledger struct{ db *pgxpool.Pool }

func NewLedger(db *pgxpool.Pool) *Ledger { return &Ledger{db: db} }

// Assign records the scheduler's next execution owner. previousGeneration is
// zero only for a new attempt; otherwise it must match the current row.
func (l *Ledger) Assign(ctx context.Context, clusterID, attemptID string, previousGeneration int64, actor Actor) (Scope, error) {
	if clusterID == "" || attemptID == "" || previousGeneration < 0 ||
		actor.Atespace == "" || actor.Name == "" || actor.UID == "" {
		return Scope{}, ErrDenied
	}
	scope := Scope{ClusterID: clusterID, AttemptID: attemptID}
	var err error
	if previousGeneration == 0 {
		err = l.db.QueryRow(ctx, `INSERT INTO bootstrap_owners
			(cluster_id, attempt_id, owner_generation, actor_atespace, actor_name, actor_uid, active)
			VALUES ($1,$2,1,$3,$4,$5,true) ON CONFLICT DO NOTHING RETURNING owner_generation`,
			clusterID, attemptID, actor.Atespace, actor.Name, actor.UID).Scan(&scope.OwnerGeneration)
	} else {
		err = l.db.QueryRow(ctx, `UPDATE bootstrap_owners SET owner_generation=owner_generation+1,
			actor_atespace=$4, actor_name=$5, actor_uid=$6, active=true
			WHERE cluster_id=$1 AND attempt_id=$2 AND owner_generation=$3 RETURNING owner_generation`,
			clusterID, attemptID, previousGeneration, actor.Atespace, actor.Name, actor.UID).Scan(&scope.OwnerGeneration)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, ErrDenied
	}
	if err != nil {
		return Scope{}, fmt.Errorf("assign bootstrap owner: %w", err)
	}
	return scope, nil
}

// Deactivate fences the current owner. A stale generation cannot stop its replacement.
func (l *Ledger) Deactivate(ctx context.Context, scope Scope) (Scope, error) {
	if !validScope(scope) {
		return Scope{}, ErrDenied
	}
	err := l.db.QueryRow(ctx, `UPDATE bootstrap_owners SET active=false,
		owner_generation=owner_generation+1 WHERE cluster_id=$1 AND attempt_id=$2
		AND owner_generation=$3 AND active RETURNING owner_generation`,
		scope.ClusterID, scope.AttemptID, scope.OwnerGeneration).Scan(&scope.OwnerGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return Scope{}, ErrDenied
	}
	if err != nil {
		return Scope{}, fmt.Errorf("deactivate bootstrap owner: %w", err)
	}
	return scope, nil
}

// Issue returns a fresh connector nonce for the current execution owner.
// Only its SHA-256 digest is persisted; a new offer cancels the old one.
func (l *Ledger) Issue(ctx context.Context, scope Scope) (Offer, error) {
	if !validScope(scope) {
		return Offer{}, ErrDenied
	}
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return Offer{}, fmt.Errorf("begin bootstrap issue: %w", err)
	}
	defer tx.Rollback(ctx)
	offer := Offer{Scope: scope}
	err = tx.QueryRow(ctx, `SELECT actor_atespace, actor_name, actor_uid
		FROM bootstrap_owners WHERE cluster_id=$1 AND attempt_id=$2
		AND owner_generation=$3 AND active FOR UPDATE`, scope.ClusterID, scope.AttemptID, scope.OwnerGeneration).
		Scan(&offer.ActorAtespace, &offer.ActorName, &offer.ActorUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Offer{}, ErrDenied
	}
	if err != nil {
		return Offer{}, fmt.Errorf("read bootstrap owner: %w", err)
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return Offer{}, fmt.Errorf("create challenge ID: %w", err)
	}
	if _, err := rand.Read(offer.Nonce[:]); err != nil {
		return Offer{}, fmt.Errorf("create challenge nonce: %w", err)
	}
	offer.ID = base64.RawURLEncoding.EncodeToString(id[:])
	nonceHash := sha256.Sum256(offer.Nonce[:])
	if _, err := tx.Exec(ctx, `UPDATE bootstrap_challenges SET cancelled_at=clock_timestamp()
		WHERE cluster_id=$1 AND attempt_id=$2 AND cancelled_at IS NULL AND consumed_at IS NULL`,
		scope.ClusterID, scope.AttemptID); err != nil {
		return Offer{}, fmt.Errorf("cancel prior bootstrap challenge: %w", err)
	}
	err = tx.QueryRow(ctx, `INSERT INTO bootstrap_challenges
		(id, cluster_id, attempt_id, owner_generation, actor_atespace, actor_name, actor_uid, nonce_sha256, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,clock_timestamp()+interval '2 minutes') RETURNING expires_at`,
		offer.ID, scope.ClusterID, scope.AttemptID, scope.OwnerGeneration,
		offer.ActorAtespace, offer.ActorName, offer.ActorUID, nonceHash[:]).Scan(&offer.ExpiresAt)
	if err != nil {
		return Offer{}, fmt.Errorf("insert bootstrap challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Offer{}, fmt.Errorf("commit bootstrap issue: %w", err)
	}
	return offer, nil
}

// Redeem verifies the actor proof and consumes one challenge while locking the
// current execution owner. The caller must still recheck AX/Substrate actor
// assignment and platform policy immediately before any release or delivery.
func (l *Ledger) Redeem(ctx context.Context, scope Scope, id string, nonce [32]byte, proof Proof, roots *x509.CertPool) (Redeemed, error) {
	if !validScope(scope) || id == "" || nonce == [32]byte{} || roots == nil {
		return Redeemed{}, ErrDenied
	}
	tx, err := l.db.Begin(ctx)
	if err != nil {
		return Redeemed{}, fmt.Errorf("begin bootstrap redeem: %w", err)
	}
	defer tx.Rollback(ctx)
	var ownerAtespace, ownerName, ownerUID string
	err = tx.QueryRow(ctx, `SELECT actor_atespace, actor_name, actor_uid FROM bootstrap_owners
		WHERE cluster_id=$1 AND attempt_id=$2 AND owner_generation=$3 AND active FOR UPDATE`,
		scope.ClusterID, scope.AttemptID, scope.OwnerGeneration).
		Scan(&ownerAtespace, &ownerName, &ownerUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Redeemed{}, ErrDenied
	}
	if err != nil {
		return Redeemed{}, fmt.Errorf("read bootstrap owner: %w", err)
	}
	var storedGeneration int64
	var storedAtespace, storedName, storedUID string
	var storedHash []byte
	var expiresAt, dbNow time.Time
	var cancelled, consumed bool
	err = tx.QueryRow(ctx, `SELECT owner_generation, actor_atespace, actor_name, actor_uid,
		nonce_sha256, expires_at, clock_timestamp(), cancelled_at IS NOT NULL, consumed_at IS NOT NULL
		FROM bootstrap_challenges WHERE id=$1 AND cluster_id=$2 AND attempt_id=$3 FOR UPDATE`,
		id, scope.ClusterID, scope.AttemptID).
		Scan(&storedGeneration, &storedAtespace, &storedName, &storedUID,
			&storedHash, &expiresAt, &dbNow, &cancelled, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Redeemed{}, ErrDenied
	}
	if err != nil {
		return Redeemed{}, fmt.Errorf("read bootstrap challenge: %w", err)
	}
	nonceHash := sha256.Sum256(nonce[:])
	if storedGeneration != scope.OwnerGeneration || storedAtespace != ownerAtespace ||
		storedName != ownerName || storedUID != ownerUID || cancelled || consumed || !expiresAt.After(dbNow) ||
		subtle.ConstantTimeCompare(storedHash, nonceHash[:]) != 1 {
		return Redeemed{}, ErrDenied
	}
	challenge, err := Verify(Expected{Roots: roots, Atespace: ownerAtespace, ActorName: ownerName,
		ActorUID: ownerUID, Nonce: nonce, Now: dbNow}, proof.Body, proof.CertificatePEM, proof.Signature)
	if err != nil {
		return Redeemed{}, ErrDenied
	}
	var redeemed Redeemed
	err = tx.QueryRow(ctx, `UPDATE bootstrap_challenges SET consumed_at=clock_timestamp()
		WHERE id=$1 AND cancelled_at IS NULL AND consumed_at IS NULL
		AND expires_at > clock_timestamp() AND $2::timestamptz > clock_timestamp()
		RETURNING consumed_at`, id, time.Unix(challenge.ExpiresAt, 0)).Scan(&redeemed.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Redeemed{}, ErrDenied
	}
	if err != nil {
		return Redeemed{}, fmt.Errorf("consume bootstrap challenge: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Redeemed{}, fmt.Errorf("commit bootstrap redeem: %w", err)
	}
	redeemed.Scope, redeemed.ActorAtespace, redeemed.ActorName, redeemed.ActorUID, redeemed.Challenge =
		scope, ownerAtespace, ownerName, ownerUID, challenge
	return redeemed, nil
}

func validScope(scope Scope) bool {
	return scope.ClusterID != "" && scope.AttemptID != "" && scope.OwnerGeneration > 0
}
