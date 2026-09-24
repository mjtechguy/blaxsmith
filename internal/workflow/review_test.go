package workflow

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func TestReviewPostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	org, other := organization(t, pool, "review-a"), organization(t, pool, "review-b")
	caller := reviewer(t, pool, org, "owner", "review-owner")
	viewer := reviewer(t, pool, org, "viewer", "review-viewer")
	outsider := reviewer(t, pool, other, "owner", "review-outsider")
	project, err := store.CreateProject(ctx, org, "review-project", "Review project")
	if err != nil {
		t.Fatal(err)
	}
	input := RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "review-run",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64),
		VerificationSHA256: strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	commit, evidence := strings.Repeat("d", 40), strings.Repeat("e", 64)
	if _, err := store.PresentForReview(ctx, org, run.ID, commit, evidence, input.VerificationSHA256); !errors.Is(err, ErrConflict) {
		t.Fatalf("unfinished execution presented: %v", err)
	}
	if _, err := store.GetCurrentReview(ctx, org, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty review visible: %v", err)
	}
	legacyInput := input
	legacyInput.LaunchKey = "unsealed-legacy"
	legacy, err := store.CreateRun(ctx, legacyInput)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET state='succeeded'
		WHERE organization_id=$1 AND id=$2`, org, legacy.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PresentForReview(ctx, org, legacy.ID, commit, evidence, input.VerificationSHA256); !errors.Is(err, ErrConflict) {
		t.Fatalf("unsealed successful run presented: %v", err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "implement", input.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	// The frozen-launch API is integrated separately; seal this fixture before dispatch.
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true
		WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.FinishAttempt(ctx, attempt, true, input.BundleSHA256); err != nil {
		t.Fatal(err)
	}
	if state, err := store.FinalizeRun(ctx, org, run.ID); err != nil || state != "succeeded" {
		t.Fatalf("execution finalization: %q, %v", state, err)
	}
	if _, err := store.PresentForReview(ctx, org, run.ID, commit, evidence, strings.Repeat("f", 64)); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed verification policy accepted: %v", err)
	}
	if _, err := store.PresentForReview(ctx, other, run.ID, commit, evidence, input.VerificationSHA256); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant presentation: %v", err)
	}
	first, err := store.PresentForReview(ctx, org, run.ID, commit, evidence, input.VerificationSHA256)
	if err != nil || first.Revision != 1 || first.Decision != nil || first.SourceCommit != input.SourceCommit ||
		first.BundleSHA256 != input.BundleSHA256 || first.VerificationSHA256 != input.VerificationSHA256 {
		t.Fatalf("frozen evidence package: %+v, %v", first, err)
	}
	if again, err := store.PresentForReview(ctx, org, run.ID, commit, evidence, input.VerificationSHA256); err != nil || again.ID != first.ID {
		t.Fatalf("duplicate package: %+v, %v", again, err)
	}
	if _, err := store.DecideReview(ctx, outsider, run.ID, first.ID, "outsider", "approve", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant approval: %v", err)
	}
	if _, err := store.DecideReview(ctx, viewer, run.ID, first.ID, "viewer", "approve", ""); !errors.Is(err, ErrReviewDenied) {
		t.Fatalf("viewer approval: %v", err)
	}
	fake := caller
	fake.SessionID = outsider.SessionID
	if _, err := store.DecideReview(ctx, fake, run.ID, first.ID, "agent", "approve", ""); !errors.Is(err, ErrReviewDenied) {
		t.Fatalf("unbound caller approval: %v", err)
	}

	var wg sync.WaitGroup
	results := make(chan ReviewDecision, 12)
	errorsSeen := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			decision, err := store.DecideReview(ctx, caller, run.ID, first.ID, "approve-once", "approve", "")
			results <- decision
			errorsSeen <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsSeen)
	var decisionID string
	for decision := range results {
		if decisionID == "" {
			decisionID = decision.ID
		}
		if decision.ID != decisionID || decision.PrincipalID != caller.PrincipalID || decision.DecidedAt.IsZero() {
			t.Fatalf("approval race identity: %+v", decision)
		}
	}
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("approval race: %v", err)
		}
	}
	var renewedSession string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_sessions
		(organization_id,id,principal_id,auth_method,mfa_level,expires_at)
		VALUES ($1,gen_random_uuid(),$2,'local','none',clock_timestamp()+interval '1 hour') RETURNING id`,
		org, caller.PrincipalID).Scan(&renewedSession); err != nil {
		t.Fatal(err)
	}
	renewed := caller
	renewed.SessionID = renewedSession
	if replay, err := store.DecideReview(ctx, renewed, run.ID, first.ID, "approve-once", "approve", ""); err != nil || replay.ID != decisionID {
		t.Fatalf("authenticated same-principal replay: %+v, %v", replay, err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, first.ID, "different-key", "approve", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("second decision accepted: %v", err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, first.ID, "approve-once", "request_changes", "Please correct the failing tests."); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotent decision accepted: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_review_decisions WHERE organization_id=$1 AND run_id=$2`, org, run.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate decision rows: %d, %v", count, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_review_packages SET evidence_sha256=$1 WHERE organization_id=$2 AND id=$3`,
		strings.Repeat("f", 64), org, first.ID); err == nil {
		t.Fatal("immutable evidence changed")
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_review_decisions SET action='request_changes' WHERE organization_id=$1 AND id=$2`,
		org, decisionID); err == nil {
		t.Fatal("immutable decision changed")
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_review_decisions SET feedback='tampered feedback' WHERE organization_id=$1 AND id=$2`,
		org, decisionID); err == nil {
		t.Fatal("immutable feedback changed")
	}

	second, err := store.PresentForReview(ctx, org, run.ID, commit, strings.Repeat("f", 64), input.VerificationSHA256)
	if err != nil || second.Revision != 2 || second.ID == first.ID || second.Decision != nil {
		t.Fatalf("evidence invalidation: %+v, %v", second, err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, first.ID, "approve-once", "approve", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale approval replay accepted: %v", err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, second.ID, "approve-once", "approve", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("old idempotency key reused: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_review_decisions
		(organization_id,run_id,package_id,idempotency_key,principal_id,session_id,action)
		VALUES ($1,$2,$3,'direct-empty-feedback',$4,$5,'request_changes')`,
		org, run.ID, second.ID, caller.PrincipalID, caller.SessionID); err == nil {
		t.Fatal("database accepted a correction without feedback")
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, second.ID, "missing-feedback", "request_changes", "  "); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty correction feedback accepted: %v", err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, second.ID, "short-feedback", "request_changes", "too short"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("vague correction feedback accepted: %v", err)
	}
	feedback := "  Please fix the failed verification and attach the passing result.  "
	if change, err := store.DecideReview(ctx, caller, run.ID, second.ID, "request-corrections", "request_changes", feedback); err != nil || change.Action != "request_changes" || change.Feedback != strings.TrimSpace(feedback) {
		t.Fatalf("request changes: %+v, %v", change, err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, second.ID, "request-corrections", "request_changes", "Please change something else instead."); !errors.Is(err, ErrConflict) {
		t.Fatalf("replayed correction changed feedback: %v", err)
	}
	if current, err := store.GetCurrentReview(ctx, org, run.ID); err != nil || current.Decision == nil || current.Decision.Feedback != strings.TrimSpace(feedback) {
		t.Fatalf("correction feedback not visible: %+v, %v", current, err)
	}
	third, err := store.PresentForReview(ctx, org, run.ID, strings.Repeat("1", 40), evidence, input.VerificationSHA256)
	if err != nil || third.Revision != 3 || third.Decision != nil {
		t.Fatalf("integrated revision invalidation: %+v, %v", third, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND id=$2`, org, caller.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideReview(ctx, caller, run.ID, third.ID, "revoked", "approve", ""); !errors.Is(err, ErrReviewDenied) {
		t.Fatalf("revoked session approved: %v", err)
	}
	fresh := reviewer(t, pool, org, "admin", "review-admin")
	if _, err := store.DecideReview(ctx, fresh, run.ID, third.ID, "fresh-approval", "approve", ""); err != nil {
		t.Fatal(err)
	}
	current, err := store.GetCurrentReview(ctx, org, run.ID)
	if err != nil || current.ID != third.ID || current.Decision == nil || current.Decision.Action != "approve" {
		t.Fatalf("current review: %+v, %v", current, err)
	}
	if _, err := store.GetCurrentReview(ctx, other, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant review lookup: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND action IN ('workflow.review.approved','workflow.review.changes_requested')`, org).Scan(&count); err != nil || count != 3 {
		t.Fatalf("decision audit: %d, %v", count, err)
	}
	events, err := store.EventsAfter(ctx, org, run.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var approvals, changes, superseded int
	for _, event := range events {
		switch event.Kind {
		case "review.approved":
			approvals++
		case "review.changes_requested":
			changes++
		case "review.superseded":
			superseded++
		}
	}
	if approvals != 2 || changes != 1 || superseded != 2 {
		t.Fatalf("review event history: approvals=%d changes=%d superseded=%d", approvals, changes, superseded)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET review_package_id=$3
		WHERE organization_id=$1 AND id=$2`, org, run.ID, first.ID); err == nil {
		t.Fatal("stale approval revived by moving review head backwards")
	}

	// Approval and replacement both lock the run. Either may win, but the
	// replacement must leave no approval effective for its new evidence.
	fourth, err := store.PresentForReview(ctx, org, run.ID, strings.Repeat("2", 40), evidence, input.VerificationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	approved := make(chan error, 1)
	replaced := make(chan struct {
		packageID string
		err       error
	}, 1)
	go func() {
		<-start
		_, err := store.DecideReview(ctx, fresh, run.ID, fourth.ID, "raced-approval", "approve", "")
		approved <- err
	}()
	go func() {
		<-start
		result, err := store.PresentForReview(ctx, org, run.ID, strings.Repeat("3", 40), evidence, input.VerificationSHA256)
		replaced <- struct {
			packageID string
			err       error
		}{result.ID, err}
	}()
	close(start)
	approvalErr, replacement := <-approved, <-replaced
	if approvalErr != nil && !errors.Is(approvalErr, ErrConflict) || replacement.err != nil {
		t.Fatalf("approve/present race: %v, %v", approvalErr, replacement.err)
	}
	current, err = store.GetCurrentReview(ctx, org, run.ID)
	if err != nil || current.ID != replacement.packageID || current.Decision != nil {
		t.Fatalf("replacement retained approval: %+v, %v", current, err)
	}
}

func reviewer(t *testing.T, pool *pgxpool.Pool, orgID, role, username string) identity.Caller {
	t.Helper()
	var principal, session string
	if err := pool.QueryRow(t.Context(), `INSERT INTO identity_principals (id,username)
		VALUES (gen_random_uuid(),$1) RETURNING id`, username).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO identity_memberships (organization_id,principal_id,role)
		VALUES ($1,$2,$3)`, orgID, principal, role); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `INSERT INTO identity_sessions
		(organization_id,id,principal_id,auth_method,mfa_level,expires_at)
		VALUES ($1,gen_random_uuid(),$2,'local','none',clock_timestamp()+interval '1 hour') RETURNING id`,
		orgID, principal).Scan(&session); err != nil {
		t.Fatal(err)
	}
	return identity.Caller{OrganizationID: orgID, PrincipalID: principal, SessionID: session,
		Role: role, AccessExpires: time.Now().Add(10 * time.Minute)}
}
