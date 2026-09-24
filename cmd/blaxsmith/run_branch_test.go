package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/runbranch"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestRunBranchDeliveryPostgres drives completion's delivery hook against a
// local bare "project repo" and a fake guest bundle reader.
func TestRunBranchDeliveryPostgres(t *testing.T) {
	pool := terminalTestPool(t)
	ctx := t.Context()
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	var org string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'run-branch','Run branch') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, org, "run-branch", "Run branch")
	if err != nil {
		t.Fatal(err)
	}
	// The Guild example committed into a repo; its bare clone is the project repo.
	root := t.TempDir()
	work := filepath.Join(root, "work")
	source, err := filepath.Abs("../../examples/guild")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(filepath.Join(work, "examples/guild"), os.DirFS(source)); err != nil {
		t.Fatal(err)
	}
	runGit(t, work, "init", "-q")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "guild")
	remotePath := filepath.Join(root, "project.git")
	runGit(t, root, "clone", "-q", "--bare", work, remotePath)
	run, err := store.CreateFrozenRun(ctx, workflow.FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: "rb",
		Source: recipe.Input{Repo: work, Ref: "HEAD", Recipe: "examples/guild/recipe.json",
			Spec: "examples/guild/spec.md", Transcript: "examples/guild/transcript.md", Scope: "examples/guild"},
		Verification: workflow.VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []workflow.VerificationCheck{
			{ID: "project-tests", Command: []string{"go", "test", "./..."}},
			{ID: "requirement-coverage", Command: []string{"verify-coverage"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded' WHERE organization_id=$1 AND run_id=$2 AND task_key='plan'`,
		org, run.ID); err != nil {
		t.Fatal(err)
	}
	var implement string
	if err := pool.QueryRow(ctx, `SELECT id FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2 AND task_key='implement'`,
		org, run.ID).Scan(&implement); err != nil {
		t.Fatal(err)
	}
	input, err := store.AttemptInput(ctx, org, run.ID, implement)
	if err != nil || input != run.SourceCommit {
		t.Fatalf("implement input: %q %v", input, err)
	}
	attempt, err := store.ReserveAttemptWithBinding(ctx, org, run.ID, implement, func(ctx context.Context, tx pgx.Tx, a workflow.Attempt) error {
		return workflow.RecordInputCommit(ctx, tx, a, input)
	})
	if err != nil {
		t.Fatal(err)
	}

	// stage makes one guest commit on the input and bundles input..HEAD like the pane.
	stage := func(name string) (string, string) {
		runGit(t, work, "checkout", "-q", "--detach", input)
		if err := os.WriteFile(filepath.Join(work, name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		runGit(t, work, "add", "-A")
		runGit(t, work, "commit", "-qm", name)
		bundle := filepath.Join(root, name+".bundle")
		runGit(t, work, "bundle", "create", "-q", bundle, input+"..HEAD")
		return runGit(t, work, "rev-parse", "HEAD"), bundle
	}
	var bundle string
	reads := 0
	delivery := &runBranchDelivery{store: store,
		readBundle: func(_ context.Context, a workflow.Attempt, w io.Writer) error {
			reads++
			if a.ID != attempt.ID || bundle == "" {
				return os.ErrNotExist
			}
			f, err := os.Open(bundle)
			if err != nil {
				return err
			}
			defer f.Close()
			_, err = io.Copy(w, f)
			return err
		},
		remote: func(_ context.Context, orgID, projectID, repoURL string) (runbranch.Remote, error) {
			if orgID != org || projectID != project {
				t.Fatalf("remote for %s/%s", orgID, projectID)
			}
			return runbranch.Remote{URL: "file://" + remotePath}, nil
		}}
	branch := func() string {
		out := runGit(t, root, "ls-remote", remotePath, runbranch.Ref(run.ID))
		tip, _, _ := strings.Cut(out, "\t")
		return tip
	}

	// No change: accepted at the input commit, nothing read or pushed.
	result := workflow.AttemptResult{Revision: input}
	if err := delivery.Deliver(ctx, attempt, &result); err != nil || result.Revision != input || reads != 0 || branch() != "" {
		t.Fatalf("no-change delivery: %+v %v reads=%d", result, err, reads)
	}
	// A new revision with no bundle is rejected.
	tip, path := stage("one")
	result = workflow.AttemptResult{Revision: tip}
	if err := delivery.Deliver(ctx, attempt, &result); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatalf("revision without bundle: %v", err)
	}
	// Tip mismatch is rejected.
	bundle = path
	result = workflow.AttemptResult{Revision: input[:39] + "0"}
	if strings.HasSuffix(input, "0") {
		result.Revision = input[:39] + "1"
	}
	if err := delivery.Deliver(ctx, attempt, &result); !errors.Is(err, workflow.ErrInvalid) || branch() != "" {
		t.Fatalf("tip mismatch: %v", err)
	}
	// The verified commit is pushed and becomes the lease expectation.
	result = workflow.AttemptResult{Revision: tip}
	if err := delivery.Deliver(ctx, attempt, &result); err != nil || result.Revision != tip || branch() != tip {
		t.Fatalf("delivery: %+v %v branch=%s", result, err, branch())
	}
	if target, err := store.GetRunBranchTarget(ctx, attempt); err != nil || target.Expected != tip || !target.CodeProducing {
		t.Fatalf("branch tip not recorded: %+v %v", target, err)
	}
	// Someone else moves the branch: the next push fails closed.
	other, _ := stage("other")
	runGit(t, work, "push", "-q", "-f", remotePath, "HEAD:"+runbranch.Ref(run.ID))
	next, nextBundle := stage("next")
	bundle = nextBundle
	result = workflow.AttemptResult{Revision: next}
	if err := delivery.Deliver(ctx, attempt, &result); !errors.Is(err, workflow.ErrInvalid) || branch() != other {
		t.Fatalf("lease conflict: %v branch=%s", err, branch())
	}
}
