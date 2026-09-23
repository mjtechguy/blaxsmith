package access

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LeaseRequest is constructed from a consumed actor proof and trusted
// assignment. The lease is only a platform delivery record; raw credentials
// can remain usable at a provider after local expiry or revocation.
type LeaseRequest struct {
	OrganizationID  string
	BindingID       string
	ChallengeID     string
	ClusterID       string
	AttemptID       string
	OwnerGeneration int64
	ActorUID        string
	RepoURL         string
	GuestExpiresAt  time.Time
}

// ReserveGitLease runs in the same transaction that commits bootstrap release
// intent, before any network send. A crash after send can then be reconciled
// against the durable attempted challenge even if delivery status is unknown.
func ReserveGitLease(ctx context.Context, tx pgx.Tx, request LeaseRequest) (string, error) {
	u, err := url.Parse(request.RepoURL)
	if tx == nil || request.OrganizationID == "" || request.BindingID == "" || request.ChallengeID == "" ||
		request.ClusterID == "" || request.AttemptID == "" || request.OwnerGeneration <= 0 ||
		request.ActorUID == "" || err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || request.GuestExpiresAt.IsZero() {
		return "", ErrDenied
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	leaseID := base64.RawURLEncoding.EncodeToString(id[:])
	err = tx.QueryRow(ctx, `INSERT INTO access_leases
		(organization_id,id,binding_id,connection_id,bootstrap_challenge_id,
		 cluster_id,attempt_id,owner_generation,actor_uid,capability,resource,audience,expires_at)
		SELECT $1,$2,b.id,g.connection_id,c.id,c.cluster_id,c.attempt_id,c.owner_generation,
		       c.actor_uid,'git.read',b.resource,$3,LEAST(c.expires_at,$4::timestamptz)
		FROM bootstrap_challenges c
		JOIN access_bindings b ON b.organization_id=$1 AND b.id=$5 AND b.attempt_id=c.attempt_id
		JOIN access_grants g ON g.organization_id=b.organization_id AND g.id=b.grant_id
		WHERE c.id=$6 AND c.cluster_id=$7 AND c.attempt_id=$8 AND c.owner_generation=$9
		AND c.actor_uid=$10 AND c.consumed_at IS NOT NULL AND c.release_attempted_at IS NOT NULL
		AND c.cancelled_at IS NULL AND c.superseded_at IS NULL AND c.released_at IS NULL
		AND b.capability='git.read' AND b.resource=$11
		AND c.expires_at>clock_timestamp() AND $4::timestamptz>clock_timestamp()
		RETURNING id`, request.OrganizationID, leaseID, "https://"+strings.ToLower(u.Host),
		request.GuestExpiresAt, request.BindingID, request.ChallengeID, request.ClusterID,
		request.AttemptID, request.OwnerGeneration, request.ActorUID, request.RepoURL).Scan(&leaseID)
	if err != nil {
		return "", deniedOrError("lease reservation", err)
	}
	return leaseID, nil
}

// MarkLeaseAttempt and MarkLeaseDelivered run in the release send transaction.
// If that transaction rolls back after network delivery, the reserved row and
// bootstrap release_attempted_at still identify an uncertain outcome.
func MarkLeaseAttempt(ctx context.Context, tx pgx.Tx, organizationID, leaseID, connectionID string, secretVersion int64) error {
	if tx == nil || organizationID == "" || leaseID == "" || connectionID == "" || secretVersion <= 0 {
		return ErrDenied
	}
	var id string
	err := tx.QueryRow(ctx, `UPDATE access_leases SET secret_version=$4,
		delivery_attempted_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND connection_id=$3
		AND secret_version IS NULL AND delivery_attempted_at IS NULL
		AND delivered_at IS NULL AND revoked_at IS NULL AND expires_at>clock_timestamp()
		RETURNING id`, organizationID, leaseID, connectionID, secretVersion).Scan(&id)
	if err != nil {
		return deniedOrError("lease attempt", err)
	}
	return nil
}

func MarkLeaseDelivered(ctx context.Context, tx pgx.Tx, organizationID, leaseID string) error {
	if tx == nil || organizationID == "" || leaseID == "" {
		return ErrDenied
	}
	var id string
	err := tx.QueryRow(ctx, `UPDATE access_leases SET delivered_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2 AND secret_version IS NOT NULL
		AND delivery_attempted_at IS NOT NULL AND delivered_at IS NULL
		AND revoked_at IS NULL AND expires_at>clock_timestamp()
		RETURNING id`, organizationID, leaseID).Scan(&id)
	if err != nil {
		return deniedOrError("lease delivery", err)
	}
	return nil
}

// RevokeGrant blocks future decisions and marks its recorded leases revoked.
// It does not invalidate a bearer token already copied into a sandbox.
func RevokeGrant(ctx context.Context, db *pgxpool.Pool, organizationID, grantID string) error {
	if db == nil || organizationID == "" || grantID == "" {
		return ErrDenied
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin grant revocation: %w", err)
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `UPDATE access_grants SET revoked_at=clock_timestamp(),version=version+1
		WHERE organization_id=$1 AND id=$2 AND revoked_at IS NULL RETURNING id`,
		organizationID, grantID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return fmt.Errorf("revoke grant: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND binding_id IN
		(SELECT id FROM access_bindings WHERE organization_id=$1 AND grant_id=$2)
		AND revoked_at IS NULL`, organizationID, grantID); err != nil {
		return fmt.Errorf("revoke recorded leases: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit grant revocation: %w", err)
	}
	return nil
}

// RevokeConnection prevents new decisions and marks every recorded delivery
// from that connection revoked. Provider-side bearer credentials still require
// provider revocation or rotation to become unusable outside this platform.
func RevokeConnection(ctx context.Context, db *pgxpool.Pool, organizationID, connectionID string) error {
	if db == nil || organizationID == "" || connectionID == "" {
		return ErrDenied
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin connection revocation: %w", err)
	}
	defer tx.Rollback(ctx)
	var id string
	err = tx.QueryRow(ctx, `UPDATE access_connections SET state='revoked'
		WHERE organization_id=$1 AND id=$2 AND state<>'revoked' RETURNING id`,
		organizationID, connectionID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return fmt.Errorf("revoke connection: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE access_leases SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND connection_id=$2 AND revoked_at IS NULL`,
		organizationID, connectionID); err != nil {
		return fmt.Errorf("revoke connection leases: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit connection revocation: %w", err)
	}
	return nil
}
