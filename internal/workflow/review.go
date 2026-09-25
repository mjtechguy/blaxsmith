package workflow

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

var ErrReviewDenied = errors.New("human review denied")

// ReviewPackage is the current, immutable evidence package for a completed run.
// A later package supersedes it; old approvals remain auditable, not effective.
type ReviewPackage struct {
	ID                 string
	OrganizationID     string
	RunID              string
	Revision           int64
	SourceCommit       string
	BundleSHA256       string
	VerificationSHA256 string
	IntegratedCommit   string
	EvidenceSHA256     string
	PresentedAt        time.Time
	Decision           *ReviewDecision
}

type ReviewDecision struct {
	ID          string
	PackageID   string
	PrincipalID string
	SessionID   string
	Action      string
	Feedback    string
	DecidedAt   time.Time
}

// PresentForReview is for the trusted scheduler after it has independently
// verified the manifest bytes, required checks, and integrated Git commit.
// Browser or agent-supplied digests are not acceptance evidence. The store
// enforces completed execution and the run's frozen verification policy.
func (s *Store) PresentForReview(ctx context.Context, orgID, runID, integratedCommit, evidenceSHA256, verificationSHA256 string) (ReviewPackage, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, runID) || !commitPattern.MatchString(integratedCommit) ||
		!hashPattern.MatchString(evidenceSHA256) || !hashPattern.MatchString(verificationSHA256) {
		return ReviewPackage{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewPackage{}, err
	}
	defer tx.Rollback(ctx)
	var state, source, bundle, frozen string
	var sealed bool
	var currentID, currentCommit, currentEvidence *string
	var revision *int64
	err = tx.QueryRow(ctx, `SELECT r.state,r.graph_sealed,r.source_commit,r.bundle_sha256,r.verification_sha256,
		r.review_package_id::text,p.revision,p.integrated_commit,p.evidence_sha256
		FROM workflow_runs r LEFT JOIN workflow_review_packages p
		ON p.organization_id=r.organization_id AND p.run_id=r.id AND p.id=r.review_package_id
		WHERE r.organization_id=$1 AND r.id=$2 FOR UPDATE OF r`, orgID, runID).
		Scan(&state, &sealed, &source, &bundle, &frozen, &currentID, &revision, &currentCommit, &currentEvidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewPackage{}, ErrNotFound
	}
	if err != nil {
		return ReviewPackage{}, err
	}
	if state != "succeeded" || !sealed || frozen != verificationSHA256 {
		return ReviewPackage{}, ErrConflict
	}
	if currentID != nil && currentCommit != nil && currentEvidence != nil &&
		*currentCommit == integratedCommit && *currentEvidence == evidenceSHA256 {
		result, err := readCurrentReview(ctx, tx, orgID, runID)
		if err != nil {
			return ReviewPackage{}, err
		}
		return result, tx.Commit(ctx)
	}
	next := int64(1)
	if revision != nil {
		next = *revision + 1
	}
	var packageID string
	err = tx.QueryRow(ctx, `INSERT INTO workflow_review_packages
		(organization_id,run_id,revision,source_commit,bundle_sha256,verification_sha256,integrated_commit,evidence_sha256)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, orgID, runID, next,
		source, bundle, frozen, integratedCommit, evidenceSHA256).Scan(&packageID)
	if err != nil {
		return ReviewPackage{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow_runs SET review_package_id=$3
		WHERE organization_id=$1 AND id=$2`, orgID, runID, packageID); err != nil {
		return ReviewPackage{}, err
	}
	if currentID != nil {
		if err := event(ctx, tx, orgID, runID, "", "", "review.superseded"); err != nil {
			return ReviewPackage{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,action,subject_id)
			VALUES ($1,'system','workflow.review.superseded',$2)`, orgID, *currentID); err != nil {
			return ReviewPackage{}, err
		}
	}
	if err := event(ctx, tx, orgID, runID, "", "", "review.presented"); err != nil {
		return ReviewPackage{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,action,subject_id)
		VALUES ($1,'system','workflow.review.presented',$2)`, orgID, packageID); err != nil {
		return ReviewPackage{}, err
	}
	result, err := readCurrentReview(ctx, tx, orgID, runID)
	if err != nil {
		return ReviewPackage{}, err
	}
	return result, tx.Commit(ctx)
}

// DecideReview requires a Caller produced by SessionManager.ValidateAccess.
// It rechecks the live human session, identity policy, and owner/admin role
// under database locks before committing one decision for the current package.
func (s *Store) DecideReview(ctx context.Context, caller identity.Caller, runID, packageID, idempotencyKey, action, feedback string) (ReviewDecision, error) {
	ctx = tenant.Org(ctx, caller.OrganizationID)
	feedback = strings.TrimSpace(feedback)
	feedbackLength := utf8.RuneCountInString(feedback)
	if !ids(caller.OrganizationID, caller.PrincipalID, caller.SessionID, runID, packageID) ||
		len(idempotencyKey) < 1 || len(idempotencyKey) > 128 ||
		(action != "approve" && action != "request_changes") ||
		(action == "approve" && feedback != "") ||
		(action == "request_changes" && (feedbackLength < 10 || feedbackLength > 4000)) {
		return ReviewDecision{}, ErrInvalid
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ReviewDecision{}, err
	}
	defer tx.Rollback(ctx)
	var state string
	var currentID *string
	err = tx.QueryRow(ctx, `SELECT state,review_package_id::text FROM workflow_runs
		WHERE organization_id=$1 AND id=$2 FOR UPDATE`, caller.OrganizationID, runID).Scan(&state, &currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewDecision{}, ErrNotFound
	}
	if err != nil {
		return ReviewDecision{}, err
	}
	if state != "succeeded" || currentID == nil || *currentID != packageID {
		return ReviewDecision{}, ErrConflict
	}
	var role string
	err = tx.QueryRow(ctx, `SELECT m.role FROM identity_sessions s
		JOIN identity_memberships m ON m.organization_id=s.organization_id AND m.principal_id=s.principal_id
		JOIN identity_principals p ON p.id=s.principal_id
		JOIN identity_organizations o ON o.id=s.organization_id
		WHERE s.organization_id=$1 AND s.id=$2 AND s.principal_id=$3
		AND m.role=$4 AND m.role IN ('owner','admin') AND $5::timestamptz>clock_timestamp()
		AND s.revoked_at IS NULL AND s.expires_at>clock_timestamp()
		AND m.state='active' AND p.state='active'
		AND (s.auth_method<>'local' OR o.login_policy IN ('local','mixed'))
		AND (o.mfa_policy<>'required' OR s.mfa_level='totp')
		FOR SHARE OF s,m,p,o`, caller.OrganizationID, caller.SessionID, caller.PrincipalID,
		caller.Role, caller.AccessExpires).Scan(&role)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewDecision{}, ErrReviewDenied
	}
	if err != nil {
		return ReviewDecision{}, err
	}
	var existing ReviewDecision
	var key string
	err = tx.QueryRow(ctx, `SELECT id,package_id,principal_id,session_id,action,feedback,decided_at,idempotency_key
		FROM workflow_review_decisions WHERE organization_id=$1 AND run_id=$2 AND
		(package_id=$3 OR idempotency_key=$4) FOR UPDATE`, caller.OrganizationID, runID, packageID, idempotencyKey).
		Scan(&existing.ID, &existing.PackageID, &existing.PrincipalID, &existing.SessionID,
			&existing.Action, &existing.Feedback, &existing.DecidedAt, &key)
	if err == nil {
		if existing.PackageID != packageID || existing.PrincipalID != caller.PrincipalID ||
			existing.Action != action || existing.Feedback != feedback || key != idempotencyKey {
			return ReviewDecision{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ReviewDecision{}, err
	}
	err = tx.QueryRow(ctx, `INSERT INTO workflow_review_decisions
		(organization_id,run_id,package_id,idempotency_key,principal_id,session_id,action,feedback)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id,decided_at`, caller.OrganizationID, runID,
		packageID, idempotencyKey, caller.PrincipalID, caller.SessionID, action, feedback).
		Scan(&existing.ID, &existing.DecidedAt)
	if err != nil {
		return ReviewDecision{}, err
	}
	existing.PackageID, existing.PrincipalID, existing.SessionID, existing.Action, existing.Feedback =
		packageID, caller.PrincipalID, caller.SessionID, action, feedback
	kind := "review.approved"
	if action == "request_changes" {
		kind = "review.changes_requested"
	}
	if err := event(ctx, tx, caller.OrganizationID, runID, "", "", kind); err != nil {
		return ReviewDecision{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO identity_audit_events
		(organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,$3,$4)`, caller.OrganizationID, caller.PrincipalID,
		"workflow."+kind, packageID); err != nil {
		return ReviewDecision{}, err
	}
	return existing, tx.Commit(ctx)
}

// GetCurrentReview never returns a superseded approval as current.
func (s *Store) GetCurrentReview(ctx context.Context, orgID, runID string) (ReviewPackage, error) {
	ctx = tenant.Org(ctx, orgID)
	if !ids(orgID, runID) {
		return ReviewPackage{}, ErrInvalid
	}
	return readCurrentReview(ctx, s.pool, orgID, runID)
}

type reviewQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func readCurrentReview(ctx context.Context, q reviewQuerier, orgID, runID string) (ReviewPackage, error) {
	var p ReviewPackage
	var decisionID, principalID, sessionID, action, feedback *string
	var decidedAt *time.Time
	err := q.QueryRow(ctx, `SELECT p.id,p.organization_id,p.run_id,p.revision,p.source_commit,p.bundle_sha256,
		p.verification_sha256,p.integrated_commit,p.evidence_sha256,p.presented_at,
		d.id::text,d.principal_id::text,d.session_id::text,d.action,d.feedback,d.decided_at
		FROM workflow_runs r JOIN workflow_review_packages p
		ON p.organization_id=r.organization_id AND p.run_id=r.id AND p.id=r.review_package_id
		LEFT JOIN workflow_review_decisions d
		ON d.organization_id=p.organization_id AND d.run_id=p.run_id AND d.package_id=p.id
		WHERE r.organization_id=$1 AND r.id=$2`, orgID, runID).
		Scan(&p.ID, &p.OrganizationID, &p.RunID, &p.Revision, &p.SourceCommit, &p.BundleSHA256,
			&p.VerificationSHA256, &p.IntegratedCommit, &p.EvidenceSHA256, &p.PresentedAt,
			&decisionID, &principalID, &sessionID, &action, &feedback, &decidedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ReviewPackage{}, ErrNotFound
	}
	if err != nil {
		return ReviewPackage{}, err
	}
	if decisionID != nil {
		p.Decision = &ReviewDecision{ID: *decisionID, PackageID: p.ID,
			PrincipalID: *principalID, SessionID: *sessionID, Action: *action, Feedback: *feedback, DecidedAt: *decidedAt}
	}
	return p, nil
}
