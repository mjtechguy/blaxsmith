package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"context"
	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Exercise the real frozen graph, attempt ownership, verification records,
// scheduler and acceptance transaction without any factory validator or AX process.
func TestNativePolicyAcceptancePostgres(t *testing.T) {
	pool := testPool(t)
	store, _ := New(pool)
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "native")
	project, err := store.CreateProject(ctx, org, "native", "Native")
	if err != nil {
		t.Fatal(err)
	}
	caller := reviewer(t, pool, org, "owner", "native-owner")
	repo := anvilRepo(t)
	for _, tc := range []struct {
		mode, acceptance string
		fail             bool
	}{
		{"off", "policy", true}, {"advisory", "policy", true}, {"required", "policy", true},
		{"required", "policy", false}, {"off", "manual", false}, {"none", "policy", false},
	} {
		t.Run(fmt.Sprintf("%s-%s-%v", tc.mode, tc.acceptance, tc.fail), func(t *testing.T) {
			r, _, fe := recipe.Validate(anvilRecipe(t))
			if fe != nil {
				t.Fatal(fe)
			}
			r.Acceptance = tc.acceptance
			r.Factory = nil
			r.Stages = r.Stages[1:]
			r.Stages[0].DependsOn = nil
			body, _ := json.Marshal(r)
			policy := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1"}
			if tc.mode != "none" {
				policy.Checks = []VerificationCheck{{ID: "tests", Command: []string{"false"}, Mode: tc.mode}}
			}
			run, err := store.CreateFrozenRun(ctx, FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: t.Name(),
				Source: recipe.Input{Repo: repo, Ref: "HEAD", Recipe: "native.json", RecipeData: body, Scope: "."}, Verification: policy})
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := store.ListRunTasks(ctx, org, run.ID)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("graph: %v %v", tasks, err)
			}
			a, err := store.ReserveAttemptWithBinding(ctx, org, run.ID, tasks[0].ID, func(ctx context.Context, tx pgx.Tx, a Attempt) error {
				return RecordInputCommit(ctx, tx, a, run.SourceCommit)
			})
			if err != nil {
				t.Fatal(err)
			}
			if err = store.ConfirmStarting(ctx, a); err != nil {
				t.Fatal(err)
			}
			if err = store.ConfirmStarted(ctx, a); err != nil {
				t.Fatal(err)
			}
			if ok, err := store.ClaimVerification(ctx, a); err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			observations := []CheckObservation{}
			outputs := [][]byte{}
			for _, check := range policy.ActiveChecks() {
				verdict, code := "pass", 0
				if tc.fail {
					verdict, code = "fail", 1
				}
				observations = append(observations, CheckObservation{Check: check.ID, Verdict: verdict, ExitCode: code})
				outputs = append(outputs, []byte("actual finding"))
			}
			if err = store.RecordVerification(ctx, a, run.SourceCommit, observations, outputs); err != nil {
				t.Fatal(err)
			}
			if err = store.ConfirmStopped(ctx, a); err != nil {
				t.Fatal(err)
			}
			if _, err = store.Progress(ctx, "", "", 100, nil); err != nil {
				t.Fatal(err)
			}
			current, err := store.GetCurrentReview(ctx, org, run.ID)
			if tc.mode == "required" && tc.fail {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("required failure accepted: %+v %v", current, err)
				}
				return
			}
			if err != nil || current.AcceptanceMode != tc.acceptance || current.Decision != nil {
				t.Fatalf("acceptance: %+v %v", current, err)
			}
			views, _, err := store.ListWorkspaceRuns(ctx, caller, RunFilter{ProjectID: project})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, view := range views {
				if view.ID == run.ID {
					found = true
					if view.ReviewWaiting != (tc.acceptance == "manual") {
						t.Fatalf("incorrect workspace review status: %+v", view)
					}
				}
			}
			if !found {
				t.Fatal("completed run disappeared from workspace")
			}
			inbox, _, err := store.ListInbox(ctx, caller, InboxFilter{ProjectID: project})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range inbox {
				if item.RunID == run.ID && tc.acceptance == "policy" {
					t.Fatal("policy acceptance appeared in the human inbox")
				}
			}
			records, err := store.ListEvidence(ctx, org, run.ID)
			if err != nil || len(records) != len(observations) {
				t.Fatalf("evidence: %v %v", records, err)
			}
			if tc.mode == "advisory" && (len(records) != 1 || !json.Valid([]byte(records[0].Metadata))) {
				t.Fatal("advisory finding lost")
			}
		})
	}
}

func TestVerificationModesCannotDowngradeRequiredChecks(t *testing.T) {
	for _, mode := range []string{"off", "advisory", "invalid"} {
		p := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{{ID: "tests", Command: []string{"true"}, Mode: mode}}}
		if _, err := validateVerification(p, []string{"tests"}); err == nil {
			t.Fatalf("required check downgraded to %s", mode)
		}
	}
}

// Scheduling consumes an already authenticated result. Signature validation is
// exercised separately by the command-exit and stage-flow integration tests.
func TestNativeReviewAndManualRepairPostgres(t *testing.T) {
	pool := testPool(t)
	store, _ := New(pool)
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "native-review")
	project, err := store.CreateProject(ctx, org, "native-review", "Native review")
	if err != nil {
		t.Fatal(err)
	}
	caller := reviewer(t, pool, org, "owner", "native-reviewer")
	repo := anvilRepo(t)
	for _, mode := range []string{"advisory", "required", "repair"} {
		r, _, fe := recipe.Validate(anvilRecipe(t))
		if fe != nil {
			t.Fatal(fe)
		}
		r.Stages = r.Stages[:1]
		r.Acceptance = "policy"
		verdict := "fail"
		if mode != "repair" {
			r.Stages[0].Kind = "review"
			r.Stages[0].Mode = mode
		} else {
			r.Acceptance = "manual"
			verdict = "pass"
		}
		body, _ := json.Marshal(r)
		run, err := store.CreateFrozenRun(ctx, FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: mode,
			Source: recipe.Input{Repo: repo, Ref: "HEAD", Recipe: "native.json", RecipeData: body, Scope: "."}, Verification: VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1"}})
		if err != nil {
			t.Fatal(err)
		}
		tasks, err := store.ListRunTasks(ctx, org, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		a, err := store.ReserveAttempt(ctx, org, run.ID, tasks[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if err = store.ConfirmStarting(ctx, a); err != nil {
			t.Fatal(err)
		}
		if err = store.ConfirmStarted(ctx, a); err != nil {
			t.Fatal(err)
		}
		result := AttemptResult{Summary: "Review found a problem", Revision: run.SourceCommit, Verdict: verdict}
		resultJSON, _ := json.Marshal(result)
		if _, err = pool.Exec(ctx, `INSERT INTO workflow_attempt_results(organization_id,attempt_id,run_id,task_id,summary,revision,verdict,result_sha256) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, org, a.ID, run.ID, a.TaskID, result.Summary, result.Revision, result.Verdict, sha(resultJSON)); err != nil {
			t.Fatal(err)
		}
		if err = store.ConfirmStopped(ctx, a); err != nil {
			t.Fatal(err)
		}
		if _, err = store.Progress(ctx, "", "", 100, nil); err != nil {
			t.Fatal(err)
		}
		pkg, err := store.GetCurrentReview(ctx, org, run.ID)
		if mode == "required" {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("required review failure accepted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode == "advisory" {
			if _, err = store.DecideReview(ctx, caller, run.ID, pkg.ID, "approve", "approve", ""); !errors.Is(err, ErrConflict) {
				t.Fatalf("policy acceptance became a human approval: %v", err)
			}
			var got string
			if err = pool.QueryRow(ctx, `SELECT verdict FROM workflow_attempt_results WHERE organization_id=$1 AND attempt_id=$2`, org, a.ID).Scan(&got); err != nil || got != "fail" {
				t.Fatalf("advisory verdict rewritten: %s %v", got, err)
			}
		} else {
			if _, err = store.DecideReview(ctx, caller, run.ID, pkg.ID, "changes", "request_changes", "Please repair the missing behavior."); err != nil {
				t.Fatal(err)
			}
			if changed, err := store.ApplyReviewDecision(ctx, org, run.ID); err != nil || !changed {
				t.Fatalf("native manual repair: %v %v", changed, err)
			}
			updated, err := store.GetRun(ctx, org, run.ID)
			if err != nil || updated.State != "active" {
				t.Fatalf("repair did not reopen: %+v %v", updated, err)
			}
		}
	}
}
