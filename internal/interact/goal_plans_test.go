package interact

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestGoalPlansPostgres(t *testing.T) {
	f := newFixture(t)
	ctx := tenant.System(t.Context())
	project, err := f.workflow.CreateProject(ctx, f.org, "plans", "Plans")
	if err != nil {
		t.Fatal(err)
	}
	goal, err := f.store.CreateGoal(ctx, f.owner, GoalInput{ProjectID: project, RequestKey: "goal", Title: "Plan", Brief: "Saved search", FactoryID: "anvil", FactoryVersion: "0.1.0"})
	if err != nil {
		t.Fatal(err)
	}
	in := GoalPlanInput{GoalID: goal, GoalRevision: 1, RequestKey: "save", Content: []byte(`{"plan":"one"}`)}
	version, err := f.store.SaveGoalPlan(ctx, f.owner, in)
	if err != nil || version != 1 {
		t.Fatalf("save: %d %v", version, err)
	}
	version, err = f.store.SaveGoalPlan(ctx, f.owner, in)
	if err != nil || version != 1 {
		t.Fatalf("retry: %d %v", version, err)
	}
	changed := in
	changed.RequestKey = "second"
	if _, err = f.store.SaveGoalPlan(ctx, f.owner, changed); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale version accepted: %v", err)
	}
	changed.ExpectedVersion = 1
	changed.Content = []byte(`{"plan":"two"}`)
	if _, err = f.store.SaveGoalPlan(ctx, f.owner, changed); err != nil {
		t.Fatal(err)
	}
	plans, more, err := f.store.GoalPlans(ctx, f.org, goal, 0)
	if err != nil || more || len(plans) != 2 || plans[1].JSON != string(in.Content) {
		t.Fatalf("history: %+v %v", plans, err)
	}
	if _, err = time.Parse(time.RFC3339Nano, plans[0].CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE workflow_goal_plans SET content='{}' WHERE goal_id=$1`, goal); err == nil {
		t.Fatal("mutable plan history")
	}
	viewer := caller(t, f.pool, f.org, "viewer", "plan-viewer")
	if _, err = f.store.SaveGoalPlan(ctx, viewer, in); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer write: %v", err)
	}
	foreign := organization(t, f.pool, "foreign-plan")
	if plans, _, err = f.store.GoalPlans(ctx, foreign, goal, 0); err != nil || len(plans) != 0 {
		t.Fatalf("cross org read: %v", err)
	}

	// Use real frozen admission, then seed a completed worker result. No model calls.
	snapshot, data, err := f.store.SnapshotGoal(ctx, f.org, goal, 1)
	if err != nil || !snapshot.References()["brief"] {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err = os.WriteFile(filepath.Join(repo, "README.md"), []byte("Existing project"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--template=", "--initial-branch=main"}, {"add", "."}, {"commit", "-m", "fixture"}} {
		base := []string{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}
		if output, err := exec.Command("git", append(base, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %s %v", output, err)
		}
	}
	source, err := f.workflow.SetProjectSourceAs(ctx, f.owner, project, "https://github.com/example/project.git", "main", "")
	if err != nil {
		t.Fatal(err)
	}
	verification, err := f.workflow.SetProjectVerificationAs(ctx, f.owner, project, 0, workflow.VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1"})
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"schema_version":"blaxsmith.recipe/v1alpha1","name":"planner","acceptance":"manual","profiles":{"p":{"harness":"codex","model":"test-model","effort":"medium"}},"documents":[".blaxsmith/platform/goal.json"],"stages":[{"id":"plan","kind":"plan","profile":"p","prompt":".blaxsmith/platform/plan.md"}],"limits":{"max_correction_cycles":0,"timeout_seconds":900}}`)
	files := map[string]recipe.PlatformFile{".blaxsmith/platform/goal.json": {Data: data, Source: "goal:" + goal + "@1"}, ".blaxsmith/platform/plan.md": {Data: []byte("Plan this goal"), Source: "factory:test@1"}}
	launch := workflow.FrozenRunInput{OrganizationID: f.org, ProjectID: project, LaunchKey: "planner", GoalID: goal, GoalRevision: 1, Caller: &f.owner, SourceRepositoryURL: source.RepositoryURL, SourceRef: source.Ref, VerificationVersion: verification.Version, Verification: verification.Policy, Source: recipe.Input{Repo: repo, Ref: "HEAD", Scope: ".", Recipe: ".blaxsmith/platform/planning.json", RecipeData: body, PlatformFiles: files}}
	run, err := f.workflow.CreateFrozenRun(ctx, launch)
	if err != nil {
		t.Fatal(err)
	}

	replay, err := f.workflow.CreateFrozenRun(ctx, launch)
	if err != nil || replay.ID != run.ID {
		t.Fatalf("launch retry: %v", err)
	}
	otherGoal, err := f.store.CreateGoal(ctx, f.owner, GoalInput{ProjectID: project, RequestKey: "other-goal", Title: "Other", Brief: "Other goal", FactoryID: "test", FactoryVersion: "1"})
	if err != nil {
		t.Fatal(err)
	}
	wrongGoal := launch
	wrongGoal.GoalID = otherGoal
	if _, err = f.workflow.CreateFrozenRun(ctx, wrongGoal); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("reused launch key changed goal association: %v", err)
	}
	runs, err := f.store.GoalRuns(ctx, f.org, goal)
	if err != nil || len(runs) != 1 || runs[0].ID != run.ID || runs[0].GoalRevision != 1 {
		t.Fatalf("association: %+v %v", runs, err)
	}

	// The generic engine records the exact immutable plan selected for a run.
	execution := launch
	execution.LaunchKey = "execution"
	execution.GoalPlanVersion = 1
	planSum := sha256.Sum256(in.Content)
	execution.GoalPlanSHA256 = hex.EncodeToString(planSum[:])
	bundle, err := f.workflow.PreviewSource(ctx, f.owner, project, execution.Source)
	if err != nil {
		t.Fatal(err)
	}
	execution.ExpectedBundleSHA256 = bundle.Digest
	execution.ExpectedVerificationSHA256, err = workflow.PreviewVerification(bundle, verification.Policy)
	if err != nil {
		t.Fatal(err)
	}
	wrong := execution
	wrong.GoalPlanSHA256 = strings.Repeat("f", 64)
	if _, err = f.workflow.CreateFrozenRun(ctx, wrong); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("wrong plan digest accepted: %v", err)
	}
	executed, err := f.workflow.CreateFrozenRun(ctx, execution)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.workflow.CreateFrozenRun(ctx, execution)
	if err != nil || again.ID != executed.ID {
		t.Fatalf("execution replay: %v", err)
	}
	wrong = execution
	wrong.GoalPlanVersion = 2
	secondSum := sha256.Sum256(changed.Content)
	wrong.GoalPlanSHA256 = hex.EncodeToString(secondSum[:])
	if _, err = f.workflow.CreateFrozenRun(ctx, wrong); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("launch key changed selected plan: %v", err)
	}
	associated, err := f.store.GoalRuns(ctx, f.org, goal)
	if err != nil || len(associated) != 2 || associated[0].PlanVersion != 1 || associated[1].PlanVersion != 0 {
		t.Fatalf("execution provenance: %+v %v", associated, err)
	}
	tasks, err := f.workflow.ListRunTasks(ctx, f.org, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var task string
	for _, v := range tasks {
		if v.Key == "plan" {
			task = v.ID
		}
	}
	a, err := f.workflow.ReserveAttempt(ctx, f.org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.workflow.ConfirmStarting(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = f.workflow.ConfirmStarted(ctx, a); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(in.Content)
	artifact := evidence.Artifact{ID: "anvil-plan", Path: "anvil-plan.json", Title: "Plan", Kind: "report", Renderer: "json", SHA256: hex.EncodeToString(sum[:])}
	if err = f.workflow.RecordArtifact(ctx, a, run.SourceCommit, artifact, in.Content); err != nil {
		t.Fatal(err)
	}
	records, err := f.workflow.ListEvidence(ctx, f.org, run.ID)
	if err != nil || len(records) != 1 {
		t.Fatalf("evidence: %v", err)
	}
	eid := records[0].ID
	if _, _, _, err = f.store.PlanEvidence(ctx, f.org, goal, eid, "anvil-plan"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("unfinished plan accepted: %v", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded',active_attempt_id=NULL WHERE organization_id=$1 AND id=$2`, f.org, task); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err = f.store.PlanEvidence(ctx, f.org, goal, eid, "anvil-plan"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("unfinished attempt accepted: %v", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE workflow_attempts SET state='succeeded' WHERE organization_id=$1 AND id=$2`, f.org, a.ID); err != nil {
		t.Fatal(err)
	}
	got, runID, revision, err := f.store.PlanEvidence(ctx, f.org, goal, eid, "anvil-plan")
	if err != nil || string(got) != string(in.Content) || runID != run.ID || revision != 1 {
		t.Fatalf("completed plan: %s %s %d %v", got, runID, revision, err)
	}
	changed = GoalPlanInput{GoalID: goal, GoalRevision: 1, ExpectedVersion: 2, RequestKey: "import", Content: got, RunID: runID, EvidenceID: eid, ArtifactKey: "anvil-plan"}
	if _, err = f.store.SaveGoalPlan(ctx, f.owner, changed); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = f.store.PlanEvidence(ctx, foreign, goal, eid, "anvil-plan"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("foreign artifact: %v", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE workflow_tasks SET generation=generation+1 WHERE organization_id=$1 AND id=$2`, f.org, task); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = f.store.PlanEvidence(ctx, f.org, goal, eid, "anvil-plan"); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("old attempt accepted: %v", err)
	}
	if err = f.store.ReplyGoal(ctx, f.owner, GoalReply{GoalID: goal, ExpectedRevision: 1, RequestKey: "context", Kind: "message", Text: "Changed scope"}); err != nil {
		t.Fatal(err)
	}
	launch.LaunchKey = "stale-planner"
	if _, err = f.workflow.CreateFrozenRun(ctx, launch); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale goal launched: %v", err)
	}
	if runs, err = f.store.GoalRuns(ctx, f.org, goal); err != nil || len(runs) != 2 {
		t.Fatalf("stale launch left a run: %v", err)
	}
	changed = in
	changed.RequestKey = "stale-goal"
	changed.ExpectedVersion = 3
	if _, err = f.store.SaveGoalPlan(ctx, f.owner, changed); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale goal plan: %v", err)
	}
	if _, _, err = f.store.SnapshotGoal(ctx, f.org, goal, 1); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale snapshot: %v", err)
	}
	if _, err = f.store.SaveGoalPlan(ctx, f.owner, in); err != nil {
		t.Fatalf("lost response retry after changes: %v", err)
	}
}
