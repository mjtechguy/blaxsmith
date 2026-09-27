package workflow

import (
	"encoding/json"
	"errors"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

const reviewTestPolicy = `{"schema_version":"blaxsmith.verification/v1alpha1","checks":[{"id":"tests","command":["true"]}]}`

// Review-specific tests seed trusted observations; actual verifier collection
// and ownership tests exercise RecordVerification separately.
func seedReviewProof(t *testing.T, pool *pgxpool.Pool, org, run, revision string) {
	t.Helper()
	ctx := tenant.System(t.Context())
	var task, attempt, policy string
	var generation int
	if err := pool.QueryRow(ctx, `SELECT t.id,t.generation,r.verification_sha256 FROM workflow_tasks t JOIN workflow_runs r ON r.organization_id=t.organization_id AND r.id=t.run_id WHERE t.organization_id=$1 AND t.run_id=$2 ORDER BY t.created_at LIMIT 1`, org, run).Scan(&task, &generation, &policy); err != nil {
		t.Fatal(err)
	}
	generation++
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_attempts(organization_id,run_id,task_id,generation,state) VALUES($1,$2,$3,$4,'succeeded') RETURNING id`, org, run, task, generation).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET generation=$3,state='succeeded',active_attempt_id=NULL WHERE organization_id=$1 AND id=$2`, org, task, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_evidence(organization_id,run_id,task_id,attempt_id,kind,origin_key,revision,policy_sha256,metadata,sha256)
 VALUES($1,$2,$3,$4,'verification','tests',$5,$6,'{"check":"tests","exit_code":0,"verdict":"pass"}',$7)`, org, run, task, attempt, revision, policy, sha(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceOwnerIsolationAndContentPostgres(t *testing.T) {
	pool := testPool(t)
	s, _ := New(pool)
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "evidence")
	other := organization(t, pool, "foreign-evidence")
	project, err := s.CreateProject(ctx, org, "evidence-project", "Evidence")
	if err != nil {
		t.Fatal(err)
	}
	run, err := s.CreateRun(ctx, RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "evidence", SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: sha([]byte(reviewTestPolicy))})
	if err != nil {
		t.Fatal(err)
	}
	task, err := s.AddTask(ctx, org, run.ID, "implement", run.BundleSHA256, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO workflow_run_bundles(organization_id,run_id,bundle_json,verification_json) VALUES($1,$2,'{}',$3::jsonb)`, org, run.ID, reviewTestPolicy); err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	a, err := s.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmStarting(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfirmStarted(ctx, a); err != nil {
		t.Fatal(err)
	}
	data := []byte("# report")
	artifact := evidence.Artifact{ID: "report", Path: "report.md", Title: "Report", Kind: "report", Renderer: "markdown", SHA256: sha(data)}
	for range 2 {
		if err = s.RecordArtifact(ctx, a, run.SourceCommit, artifact, data); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.RecordArtifact(ctx, a, run.SourceCommit, artifact, []byte("changed")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("digest mismatch: %v", err)
	}
	stale := a
	stale.OwnerGeneration++
	if err = s.RecordArtifact(ctx, stale, run.SourceCommit, artifact, data); !errors.Is(err, ErrFenced) {
		t.Fatalf("stale owner: %v", err)
	}
	records, err := s.ListEvidence(ctx, org, run.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("idempotence: %+v %v", records, err)
	}
	got, mime, err := s.EvidenceContent(ctx, org, run.ID, records[0].ID)
	if err != nil || string(got) != string(data) || mime != "text/plain; charset=utf-8" {
		t.Fatalf("content: %s %s %v", got, mime, err)
	}
	if _, _, err = s.EvidenceContent(ctx, other, run.ID, records[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign content: %v", err)
	}
	if _, err = pool.Exec(ctx, `UPDATE workflow_evidence SET content='changed' WHERE organization_id=$1`, org); err == nil {
		t.Fatal("immutable evidence changed")
	}
	if _, err = s.PresentForReview(ctx, org, run.ID, run.SourceCommit, sha(nil), run.VerificationSHA256); !errors.Is(err, ErrConflict) {
		t.Fatalf("running review: %v", err)
	}
	if err = s.RequestCancel(ctx, org, run.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.RecordArtifact(ctx, a, run.SourceCommit, artifact, data); !errors.Is(err, ErrFenced) {
		t.Fatalf("cancelled owner: %v", err)
	}
}

func TestEvidenceMediaAndPolicy(t *testing.T) {
	a := evidence.Artifact{ID: "x", Path: "x", Title: "x", Kind: "image", Renderer: "image"}
	data := []byte(`<svg onload="alert(1)"></svg>`)
	a.SHA256 = sha(data)
	if _, err := ArtifactMIME(a, data); err == nil {
		t.Fatal("active image accepted")
	}
	p := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{{ID: "test", Command: []string{"true"}, TrustedPaths: []string{"../escape"}}}}
	if _, err := validateVerification(p, nil); err == nil {
		t.Fatal("unsafe protected path")
	}

}

func reviewTestBundle(key string) ([]byte, string) {
	b := recipe.Bundle{SchemaVersion: "blaxsmith.bundle/v1alpha1", Source: recipe.Source{Commit: strings.Repeat("a", 40), Scope: "."}, Recipe: recipe.Recipe{Acceptance: "manual", Profiles: map[string]recipe.Profile{"p": {Harness: "codex", Model: "gpt-5"}}, Stages: []recipe.Stage{{ID: key, Kind: "implement", Profile: "p"}}}}
	data, _ := json.Marshal(b)
	b.Digest = sha(data)
	data, _ = json.Marshal(b)
	return data, b.Digest
}
