package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/internal/anvil"
	"github.com/mjtechguy/blaxsmith/internal/dispatch"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/runbranch"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// Qualify compiled Anvil recipes against the real ledger and local Git/checks.
// The harness and AX observations are simulated; this is not live AX qualification.
func TestAnvilExecutionCorrectionPostgres(t *testing.T) {
	pool := terminalTestPool(t)
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	var org string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations(id,slug,name) VALUES(gen_random_uuid(),'anvil-execution','Anvil execution') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, org, "anvil-execution", "Anvil execution")
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256(nil)
	emptySHA := hex.EncodeToString(empty[:])
	collector := workflow.CommandExitCollector{Store: store, SignerID: "fixture/connector", PublicKey: public, WorkerPool: "fixture"}
	plan, err := json.Marshal(anvil.Plan{Schema: anvil.PlanSchema, Title: "Repair greeting", Summary: "Preserve the existing check",
		Decisions:    []anvil.Decision{{ID: "D1", Question: "How to preserve behavior?", Choice: "Reuse the existing check", Reason: "Avoid changing the contract", Authority: "proposal", Sources: []string{"brief"}}},
		Unknowns:     []anvil.Unknown{{ID: "U1", Description: "Which deployment target?", Disposition: "blocking", Reason: "The user owns delivery scope", Sources: []string{"brief"}}},
		Requirements: []anvil.Requirement{{ID: "R1", Description: "Greeting is hello", Sources: []string{"brief"}, Examples: []string{"greeting.txt contains hello"}}},
		Phases:       []anvil.Phase{{ID: "P1", Title: "Implement", Outcome: "Passing greeting check"}},
		Tasks:        []anvil.Task{{ID: "T1", Title: "Fix greeting", Phase: "P1", Reason: "Meet the requested behavior", RequirementIDs: []string{"R1"}, Instructions: "Update greeting.txt without changing check.sh", Acceptance: []string{"check.sh exits zero"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, acceptance, mode    string
		cycles                    int
		repair                    bool
		reviewMode, reviewVerdict string
	}{
		{name: "disabled", acceptance: "policy", mode: "required"},
		{name: "repair-policy", acceptance: "policy", mode: "required", cycles: 2, repair: true},
		{name: "repair-manual", acceptance: "manual", mode: "required", cycles: 1, repair: true},
		{name: "exhausted", acceptance: "policy", mode: "required", cycles: 1},
		{name: "advisory", acceptance: "policy", mode: "advisory"},
		{name: "review-repair", acceptance: "policy", mode: "required", cycles: 1, repair: true, reviewMode: "required", reviewVerdict: "pass"},
		{name: "review-pass", acceptance: "policy", mode: "required", repair: true, reviewMode: "required", reviewVerdict: "pass"},
		{name: "review-fail", acceptance: "policy", mode: "required", repair: true, reviewMode: "required", reviewVerdict: "fail"},
		{name: "review-missing-report", acceptance: "policy", mode: "required", repair: true, reviewMode: "required", reviewVerdict: "pass"},
		{name: "review-contradiction", acceptance: "policy", mode: "required", repair: true, reviewMode: "required", reviewVerdict: "pass"},
		{name: "review-missing", acceptance: "policy", mode: "required", repair: true, reviewMode: "required"},
		{name: "review-advisory", acceptance: "policy", mode: "required", repair: true, reviewMode: "advisory", reviewVerdict: "fail"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			git := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("git %v: %s %v", args, out, err)
				}
				return strings.TrimSpace(string(out))
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			inputs := `{"schema_version":"blaxsmith.inputs/v1alpha1","rules":[{"id":"greeting","scope":".","file":"rules.md","settings":{"style":"existing"}},{"id":"scoped","scope":"src","file":"scoped.md"},{"id":"sibling","scope":"other","file":"sibling.md"}],"stack":{"name":"Greeting shell","package_manager":"npm@11.0.0","commands":[{"id":"greeting","command":["sh","check.sh"]}],"prerequisites":["POSIX shell"]},"agents":{"implementer":{"responsibility":"Preserve the greeting contract","instructions":["role.md"],"harnesses":["codex"],"models":["fixture"],"capabilities":["read_repository","write_candidate"],"completion_criteria":["Deliver a candidate and real evidence"]},"unused":{"responsibility":"UNSELECTED_AGENT_INSTRUCTIONS","completion_criteria":["Unused"]}}}`
			write("inputs.json", inputs)
			write("rules.md", "Preserve the existing greeting behavior.")
			write("role.md", "Follow the implementation role.")
			write("scoped.md", "Scoped source guidance.")
			write("sibling.md", "Sibling-only guidance.")
			write("package.json", `{"packageManager":"npm@11.0.0","engines":{"node":">=24"}}`)
			if err := os.Mkdir(filepath.Join(repo, "src"), 0700); err != nil {
				t.Fatal(err)
			}
			write("src/README.md", "Scoped fixture")
			write("AGENTS.md", "Keep the greeting check unchanged.")
			write("greeting.txt", "initial\n")
			write("check.sh", "test \"$(cat greeting.txt)\" = hello || { echo 'greeting must be hello'; exit 1; }\n")
			git("init", "--template=", "--initial-branch=main")
			git("add", ".")
			git("commit", "-m", "baseline")
			sourceCommit := git("rev-parse", "HEAD")
			knowledge, _ := json.Marshal(recipe.KnowledgeSnapshot{Schema: "blaxsmith.knowledge/v1alpha1", Title: "Greeting behavior map", SourceCommit: sourceCommit, Scope: ".", Dependencies: map[string]string{"greeting.txt": evidence.SHA([]byte("initial\n"))}, Claims: []recipe.KnowledgeClaim{{ID: "greeting", Statement: "OBSERVED_INITIAL_GREETING", Kind: "observed", Locations: []recipe.CodeLocation{{Path: "greeting.txt", Line: 1}}}}, Uncertainties: []string{"No baseline test outcome yet"}})
			write("knowledge.json", string(knowledge))
			inputs = strings.Replace(inputs, `"rules":`, `"knowledge":["knowledge.json"],"project_mode":"brownfield","rules":`, 1)
			write("inputs.json", inputs)
			git("add", "inputs.json", "knowledge.json")
			git("commit", "-m", "project knowledge")
			remote := filepath.Join(t.TempDir(), "project.git")
			git("clone", "--bare", repo, remote)
			execution := anvil.ExecutionInput{GoalID: "fixture", GoalRevision: 1, PlanVersion: 1, PlanJSON: plan,
				Context: []byte(`{"brief":"Fix the greeting"}`), References: map[string]bool{"brief": true},
				Profile: recipe.Profile{Harness: "codex", Model: "fixture", Effort: "medium", Inputs: "inputs.json", Agent: "implementer"}, RuntimeSeconds: 60, CorrectionCycles: tc.cycles, Acceptance: tc.acceptance, ReviewMode: tc.reviewMode, Reviewer: recipe.Profile{Harness: "codex", Model: "independent-review-model", Effort: "medium"}}
			body, files, _, err := anvil.ExecutionRecipe(execution)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "disabled" {
				execution.ReadinessMode = "required"
				if _, _, _, err := anvil.ExecutionRecipe(execution); err == nil || !strings.Contains(err.Error(), "U1") {
					t.Fatalf("required readiness bypassed: %v", err)
				}
				execution.ReadinessMode = "off"
				_, offFiles, _, err := anvil.ExecutionRecipe(execution)
				if err != nil {
					t.Fatal(err)
				}
				if string(offFiles[".blaxsmith/platform/readiness.json"].Data) == string(files[".blaxsmith/platform/readiness.json"].Data) {
					t.Fatal("readiness selection not frozen")
				}
				input := recipe.Input{Repo: repo, Ref: "HEAD", Recipe: anvil.ExecutionRecipePath, RecipeData: body, PlatformFiles: files, Scope: "src"}
				baseline := git("rev-parse", "HEAD")
				bundle, err := recipe.Freeze(ctx, input)
				if err != nil {
					t.Fatal(err)
				}
				if bundle.Baseline.Mode != "mixed" {
					t.Fatalf("new scoped component: %+v", bundle.Baseline)
				}
				if len(bundle.ProfileInputs["implementer"].Rules) != 2 {
					t.Fatal("sibling scoped rule leaked")
				}
				for _, a := range bundle.Artifacts {
					if a.Path == "sibling.md" {
						t.Fatal("sibling bytes included")
					}
				}
				for _, bad := range []struct{ old, new, want string }{
					{`"style":"existing"`, `"style":"existing"},"unexpected":{"x":"y"`, "unknown field"},
					{`"scope":"src","file":"scoped.md"`, `"scope":"src","file":"scoped.md","settings":{"style":"different"}`, "conflicting setting"},
					{`"write_candidate"`, `"approve_delivery"`, "unsupported capability"},
					{`"models":["fixture"]`, `"models":["different"]`, "selected model"},
					{`"package_manager":"npm@11.0.0"`, `"package_manager":"pnpm@10.0.0"`, "repository manifests/lockfiles"},
					{`"file":"scoped.md"`, `"file":"../outside"`, "scoped rule"},
				} {
					write("inputs.json", strings.Replace(inputs, bad.old, bad.new, 1))
					git("add", "inputs.json")
					git("commit", "-m", "invalid inputs")
					if _, err := recipe.Freeze(ctx, input); err == nil || !strings.Contains(err.Error(), bad.want) {
						t.Fatalf("wanted %s, got %v", bad.want, err)
					}
				}
				git("checkout", "--detach", baseline)
				input.Scope = "."
				for _, change := range []struct{ path, body, status string }{{"role.md", "Unrelated role edit", "current dependencies"}, {"greeting.txt", "Changed interface", "stale"}} {
					write(change.path, change.body)
					git("add", change.path)
					git("commit", "-m", "dependency freshness")
					f, err := recipe.Freeze(ctx, input)
					if err != nil {
						t.Fatal(err)
					}
					k := f.ProfileInputs["implementer"].Knowledge[0]
					if k.Status != change.status || (k.Status == "stale" && len(k.Claims) != 0) {
						t.Fatalf("knowledge freshness: %+v", k)
					}
					if f.Baseline.Mode != "brownfield" || f.Baseline.Commit != f.Source.Commit {
						t.Fatalf("baseline: %+v", f.Baseline)
					}
				}
				git("checkout", "--detach", baseline)
			}
			policy := workflow.VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []workflow.VerificationCheck{{ID: "greeting", Command: []string{"sh", "check.sh"}, Mode: tc.mode, TrustedPaths: []string{"check.sh"}}}}
			launchInput := workflow.FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: tc.name,
				Source: recipe.Input{Repo: repo, Ref: "HEAD", Recipe: anvil.ExecutionRecipePath, RecipeData: body, PlatformFiles: files, Scope: "."}, Verification: policy}
			var owner identity.Caller
			if tc.name == "repair-policy" {
				owner = identity.Caller{OrganizationID: org, Role: "owner", AccessExpires: time.Now().Add(time.Hour)}
				if err := pool.QueryRow(ctx, `INSERT INTO identity_principals(id,username) VALUES(gen_random_uuid(),'checkpoint-owner') RETURNING id`).Scan(&owner.PrincipalID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO identity_memberships(organization_id,principal_id,role) VALUES($1,$2,'owner')`, org, owner.PrincipalID); err != nil {
					t.Fatal(err)
				}
				if err := pool.QueryRow(ctx, `INSERT INTO identity_sessions(organization_id,id,principal_id,auth_method,mfa_level,expires_at) VALUES($1,gen_random_uuid(),$2,'local','none',clock_timestamp()+interval '1 hour') RETURNING id`, org, owner.PrincipalID).Scan(&owner.SessionID); err != nil {
					t.Fatal(err)
				}
				interactions, _ := interact.New(pool)
				goal, err := interactions.CreateGoal(ctx, owner, interact.GoalInput{ProjectID: project, RequestKey: "checkpoint-goal", Title: "Greeting", Brief: "Preserve greeting", FactoryID: "anvil", FactoryVersion: anvil.Version})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := interactions.SaveGoalPlan(ctx, owner, interact.GoalPlanInput{GoalID: goal, GoalRevision: 1, RequestKey: "first-plan", Content: plan}); err != nil {
					t.Fatal(err)
				}
				source, err := store.SetProjectSourceAs(ctx, owner, project, "https://github.com/example/greeting", "main", "")
				if err != nil {
					t.Fatal(err)
				}
				verification, err := store.SetProjectVerificationAs(ctx, owner, project, 0, policy)
				if err != nil {
					t.Fatal(err)
				}
				launchInput.Caller, launchInput.SourceRepositoryURL, launchInput.SourceRef, launchInput.VerificationVersion = &owner, source.RepositoryURL, source.Ref, verification.Version
				launchInput.GoalID, launchInput.GoalRevision, launchInput.GoalPlanVersion, launchInput.GoalPlanSHA256 = goal, 1, 1, evidence.SHA(plan)
			}
			run, err := store.CreateFrozenRun(ctx, launchInput)
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := store.ListRunTasks(ctx, org, run.ID)
			wantTasks := 2
			if tc.reviewMode != "" {
				wantTasks++
			}
			if err != nil || len(tasks) != wantTasks {
				t.Fatalf("compiled graph: %v %v", tasks, err)
			}
			ids := map[string]string{}
			for _, task := range tasks {
				ids[task.Key] = task.ID
			}
			frozen, err := store.LoadFrozenTask(ctx, org, run.ID, ids["implement"])
			if err != nil {
				t.Fatal(err)
			}
			frozen.RepositoryURL = "https://github.com/example/greeting"
			frozen.SourceRef = "main"
			approved := workflow.ApprovedToolRuntime{Runtime: tooladapter.Runtime{Harness: "codex", Image: "runner@sha256:" + strings.Repeat("a", 64), Binary: "/opt/blaxsmith/bin/codex", BinarySHA256: strings.Repeat("b", 64), Version: "0.156.1", Supported: []tooladapter.ModelEffort{{Model: "fixture", Effort: "medium"}}}, MaxTimeoutSeconds: 60, MaxOutputBytes: 1024}
			request, err := dispatch.PrepareToolRequest(frozen, approved, "", run.SourceCommit)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(request.Prompt, "OBSERVED_INITIAL_GREETING") || !strings.Contains(request.Prompt, "Follow the implementation role") || !strings.Contains(request.Prompt, "Rule directories: src") || strings.Contains(request.Prompt, "UNSELECTED_AGENT_INSTRUCTIONS") {
				t.Fatalf("resolved worker prompt: %s", request.Prompt)
			}
			candidateRequest, err := dispatch.PrepareToolRequest(frozen, approved, "", strings.Repeat("f", 40))
			if err != nil || strings.Contains(candidateRequest.Prompt, "OBSERVED_INITIAL_GREETING") || !strings.Contains(candidateRequest.Prompt, "not revalidated for candidate") {
				t.Fatalf("stale candidate knowledge leaked: %v", err)
			}
			// Editing the checkout cannot mutate a previously admitted run.
			write("role.md", "Changed after admission")
			reloaded, err := store.LoadFrozenTask(ctx, org, run.ID, ids["implement"])
			if err != nil {
				t.Fatal(err)
			}
			reloaded.RepositoryURL = frozen.RepositoryURL
			reloaded.SourceRef = frozen.SourceRef
			rebuilt, err := dispatch.PrepareToolRequest(reloaded, approved, "", run.SourceCommit)
			if err != nil || rebuilt.Prompt != request.Prompt {
				t.Fatalf("frozen guidance changed: %v", err)
			}
			git("restore", "role.md")
			bundlePath := filepath.Join(t.TempDir(), "candidate.bundle")
			delivery := &runBranchDelivery{store: store,
				readBundle: func(_ context.Context, _ workflow.Attempt, w io.Writer) error {
					f, err := os.Open(bundlePath)
					if err != nil {
						return err
					}
					defer f.Close()
					_, err = io.Copy(w, f)
					return err
				},
				remote: func(context.Context, string, string, string) (runbranch.Remote, error) {
					return runbranch.Remote{URL: "file://" + remote}, nil
				},
			}
			candidate := run.SourceCommit
			complete := func(stage, greeting string) {
				t.Helper()
				input, err := store.AttemptInput(ctx, org, run.ID, ids[stage])
				if err != nil || input != candidate {
					t.Fatalf("%s lost candidate %s: %s %v", stage, candidate, input, err)
				}
				handoff, corrections, err := store.BuildHandoff(ctx, org, run.ID, ids[stage])
				if err != nil {
					t.Fatal(err)
				}
				if stage == "implement" && input != run.SourceCommit && (len(corrections) != 1 || !strings.Contains(handoff, func() string {
					if tc.reviewMode != "" {
						return "Independent finding"
					}
					return "greeting must be hello"
				}())) {
					t.Fatalf("repair lost check findings: %s", handoff)
				}
				a, err := store.ReserveAttemptWithBinding(ctx, org, run.ID, ids[stage], func(ctx context.Context, tx pgx.Tx, a workflow.Attempt) error {
					if err := workflow.RecordHandoff(ctx, tx, a, handoff, corrections); err != nil {
						return err
					}
					return workflow.RecordInputCommit(ctx, tx, a, input)
				})
				if err != nil {
					t.Fatal(err)
				}
				binding := workflow.RuntimeBinding{AXAtespace: "fixture", AXTask: "attempt-" + a.ID, ActorUID: "actor-" + a.ID, TemplateUID: "template", Image: "runner@sha256:" + strings.Repeat("a", 64), WorkerPool: "fixture", CommandSHA256: strings.Repeat("b", 64)}
				for _, err := range []error{store.BindRuntime(ctx, a, binding), store.ConfirmStarting(ctx, a), store.ConfirmStarted(ctx, a)} {
					if err != nil {
						t.Fatal(err)
					}
				}
				git("checkout", "--detach", input)
				var result workflow.AttemptResult
				if stage == "implement" {
					write("greeting.txt", greeting+"\n")
					git("add", "greeting.txt")
					git("commit", "--allow-empty", "-m", "candidate")
					candidate = git("rev-parse", "HEAD")
					git("bundle", "create", bundlePath, input+"..HEAD")
					result = workflow.AttemptResult{Summary: "Updated greeting", Revision: candidate}
					if err := delivery.Deliver(ctx, a, &result); err != nil {
						t.Fatal(err)
					}
					if tip := git("--git-dir="+remote, "rev-parse", runbranch.Ref(run.ID)); tip != candidate {
						t.Fatalf("delivered branch %s, want %s", tip, candidate)
					}
				} else if stage == "review" {
					result = workflow.AttemptResult{Summary: "Independent finding", Revision: input, Verdict: greeting}
				} else {
					if ok, err := store.ClaimVerification(ctx, a); err != nil || !ok {
						t.Fatalf("verification claim: %v %v", ok, err)
					}
					check := policy.Checks[0]
					cmd := exec.CommandContext(t.Context(), check.Command[0], check.Command[1:]...)
					cmd.Dir = repo
					output, err := cmd.CombinedOutput()
					code, verdict := 0, "pass"
					if err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) {
							t.Fatal(err)
						}
						code, verdict = exit.ExitCode(), "fail"
					}
					if err := store.RecordVerification(ctx, a, input, []workflow.CheckObservation{{Check: check.ID, Verdict: verdict, ExitCode: code}}, [][]byte{output}); err != nil {
						t.Fatal(err)
					}
				}
				if stage != "verify" {
					if err := store.RecordAttemptResult(ctx, a, result); !errors.Is(err, workflow.ErrConflict) {
						t.Fatalf("unsigned result accepted: %v", err)
					}
					signed, err := runnerexit.Sign(runnerexit.ExitReport{Schema: runnerexit.Schema, OrganizationID: org, RunID: run.ID, TaskID: a.TaskID, AttemptID: a.ID,
						OwnerGeneration: a.OwnerGeneration, AXAtespace: binding.AXAtespace, AXTask: binding.AXTask, ActorUID: binding.ActorUID, TemplateUID: binding.TemplateUID,
						ActivationNonce: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), CommandSHA256: binding.CommandSHA256, Sequence: 1, EvidenceSHA256: emptySHA, ObservedAt: time.Now().UnixNano()}, private)
					if err != nil {
						t.Fatal(err)
					}
					if err := collector.Record(ctx, signed); err != nil {
						t.Fatal(err)
					}
					if stage == "review" {
						if tc.name != "review-missing-report" {
							verdict := result.Verdict
							if verdict == "" {
								verdict = "pass"
							}
							if tc.name == "review-contradiction" {
								verdict = "fail"
							}
							report := evidence.ReviewReport{Schema: "blaxsmith.review/v1alpha1", Candidate: input, Verdict: verdict, Summary: "Independent assessment", Requirements: []evidence.RequirementAssessment{{ID: "R1", Assessment: "met", Evidence: "Inspected greeting.txt and check.sh"}}}
							data, _ := json.Marshal(report)
							artifact := evidence.Artifact{ID: "anvil-review", Path: "anvil-review.json", Kind: "report", Title: "Independent review", Renderer: "json", SHA256: evidence.SHA(data)}
							if err := store.RecordArtifact(ctx, a, input, artifact, data); err != nil {
								t.Fatal(err)
							}
						}
						bad := result
						bad.Revision = strings.Repeat("f", 40)
						if err := store.RecordAttemptResult(ctx, a, bad); !errors.Is(err, workflow.ErrConflict) {
							t.Fatalf("review accepted stale candidate: %v", err)
						}
					}
					if err := store.RecordAttemptResult(ctx, a, result); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.ConfirmStopped(ctx, a); err != nil {
					t.Fatal(err)
				}
			}
			initial := "wrong"
			if tc.reviewMode != "" {
				initial = "hello"
			}
			complete("implement", initial)
			complete("verify", "")
			if tc.name == "repair-policy" {
				interactions, _ := interact.New(pool)
				if err := interactions.SetGoalAllowance(ctx, owner, launchInput.GoalID, 0, 20, 30, nil, 2, 60, false); err != nil {
					t.Fatal(err)
				}
				firstFailure, err := store.GetGoalAllowance(ctx, org, launchInput.GoalID)
				if err != nil || firstFailure.AdmissionClosed || firstFailure.RepeatedCheckFailures != 1 {
					t.Fatalf("stall fired before threshold: %+v %v", firstFailure, err)
				}
				complete("implement", "still wrong")
				complete("verify", "")
				limits, err := store.GetGoalAllowance(ctx, org, launchInput.GoalID)
				if err != nil || !limits.AdmissionClosed || limits.RepeatedCheckFailures != 2 || !strings.Contains(limits.StallReason, "greeting") {
					t.Fatalf("failure stall missing: %+v %v", limits, err)
				}
				restarted, _ := workflow.New(pool)
				if _, err := restarted.ReserveAttempt(ctx, org, run.ID, ids["implement"]); !errors.Is(err, workflow.ErrGoalAllowance) {
					t.Fatalf("replacement bypassed stall: %v", err)
				}
				repeatedRun := launchInput
				repeatedRun.LaunchKey = "blocked-by-stall"
				if _, err := restarted.CreateFrozenRun(ctx, repeatedRun); !errors.Is(err, workflow.ErrGoalAllowance) {
					t.Fatalf("new run bypassed stall: %v", err)
				}
				if err := interactions.SetGoalAllowance(ctx, owner, launchInput.GoalID, 1, 20, 30, nil, 2, 60, true); err != nil {
					t.Fatal(err)
				}
				recovered, err := restarted.GetGoalAllowance(ctx, org, launchInput.GoalID)
				if err != nil || recovered.AdmissionClosed || recovered.RepeatedCheckFailures != 0 || recovered.StallResetAt == nil || recovered.Attempts != limits.Attempts || recovered.Runs != limits.Runs {
					t.Fatalf("recovery erased history or remained blocked: %+v %v", recovered, err)
				}
				// Old queued work is not progress; seed its original admission time, never sleep or alter the clock.
				timedGoal, err := interactions.CreateGoal(ctx, owner, interact.GoalInput{ProjectID: project, RequestKey: "timed-goal", Title: "No progress", Brief: "Wait for evidence", FactoryID: "anvil", FactoryVersion: anvil.Version})
				if err != nil {
					t.Fatal(err)
				}
				timedInput := launchInput
				timedInput.Caller, timedInput.GoalID, timedInput.GoalRevision, timedInput.GoalPlanVersion, timedInput.GoalPlanSHA256, timedInput.LaunchKey = nil, "", 0, 0, "", "timed-run"
				timedRun, err := store.CreateFrozenRun(ctx, timedInput)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `INSERT INTO workflow_goal_runs(organization_id,goal_id,run_id,goal_revision,created_at) VALUES($1,$2,$3,1,clock_timestamp()-interval '2 minutes')`, org, timedGoal, timedRun.ID); err != nil {
					t.Fatal(err)
				}
				if err := interactions.SetGoalAllowance(ctx, owner, timedGoal, 0, 0, 0, nil, 0, 60, false); err != nil {
					t.Fatal(err)
				}
				if limits, err := store.GetGoalAllowance(ctx, org, timedGoal); err != nil || !limits.AdmissionClosed || !strings.Contains(limits.StallReason, "60 seconds") {
					t.Fatalf("no-progress boundary: %+v %v", limits, err)
				}
				timedTasks, err := store.ListRunTasks(ctx, org, timedRun.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := store.ReserveAttempt(ctx, org, timedRun.ID, timedTasks[0].ID); !errors.Is(err, workflow.ErrGoalAllowance) {
					t.Fatalf("no-progress admission: %v", err)
				}
				if err := interactions.SetGoalAllowance(ctx, owner, timedGoal, 1, 0, 0, nil, 0, 60, true); err != nil {
					t.Fatal(err)
				}
				if limits, err := store.GetGoalAllowance(ctx, org, timedGoal); err != nil || limits.AdmissionClosed || limits.Runs != 1 || limits.LastProgressAt == nil {
					t.Fatalf("timed recovery: %+v %v", limits, err)
				}
			}
			if tc.cycles > 0 && tc.mode == "required" && tc.reviewMode == "" {
				if _, err := store.GetCurrentReview(ctx, org, run.ID); !errors.Is(err, workflow.ErrNotFound) {
					t.Fatalf("failed candidate presented for acceptance: %v", err)
				}
				greeting := "still wrong"
				if tc.repair {
					greeting = "hello"
				}
				complete("implement", greeting)
				complete("verify", "")
			}
			if _, err := store.Progress(ctx, "", "", 100, nil); err != nil {
				t.Fatal(err)
			}
			if tc.reviewMode != "" {
				if tc.name == "review-repair" {
					complete("review", "fail")
					complete("implement", "hello")
					complete("verify", "")
				}
				complete("review", tc.reviewVerdict)
				if _, err := store.Progress(ctx, "", "", 100, nil); err != nil {
					t.Fatal(err)
				}
			}
			accepted := (tc.repair || tc.mode == "advisory") && !(tc.reviewMode == "required" && (tc.reviewVerdict != "pass" || tc.name == "review-missing-report" || tc.name == "review-contradiction"))
			pkg, err := store.GetCurrentReview(ctx, org, run.ID)
			if accepted {
				if err != nil || pkg.IntegratedCommit != candidate || pkg.AcceptanceMode != tc.acceptance {
					t.Fatalf("candidate acceptance: %+v %v", pkg, err)
				}
			} else {
				if !errors.Is(err, workflow.ErrNotFound) {
					t.Fatalf("failed candidate accepted: %+v %v", pkg, err)
				}
				var state string
				if err := pool.QueryRow(ctx, `SELECT state FROM workflow_tasks WHERE organization_id=$1 AND id=$2`, org, ids[func() string {
					if tc.reviewMode != "" {
						return "review"
					}
					return "verify"
				}()]).Scan(&state); err != nil {
					t.Fatal(err)
				}
				want := "blocked"
				if tc.cycles > 0 {
					want = "escalated"
				}
				if state != want {
					t.Fatalf("limit: verify=%s, want %s", state, want)
				}
			}

			report, reportErr := store.DeliveryReport(ctx, org, run.ID)
			if reportErr != nil {
				t.Fatal(reportErr)
			}
			digest := sha256.Sum256([]byte(report.Markdown))
			if report.SHA256 != hex.EncodeToString(digest[:]) || !strings.Contains(report.Markdown, "Token usage and cost: unknown") || strings.Contains(report.Markdown, "greeting must be hello") {
				t.Fatalf("invalid or raw-log report: %s", report.Markdown)
			}
			if accepted {
				if !strings.Contains(report.Markdown, candidate) {
					t.Fatal("report lost reviewed candidate")
				}
			} else if !strings.Contains(report.Markdown, "No current acceptance package") {
				t.Fatal("report falsely claimed acceptance")
			}
			if tc.repair && tc.cycles > 0 && !strings.Contains(report.Markdown, "historical attempt") {
				t.Fatal("report lost failed attempt history")
			}
			if tip := git("--git-dir="+remote, "rev-parse", "refs/heads/main"); tip != run.SourceCommit {
				t.Fatal("implementation changed the target branch")
			}
			var attempts int
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE organization_id=$1 AND task_id=$2`, org, ids["implement"]).Scan(&attempts); err != nil {
				t.Fatal(err)
			}
			want := 1
			if tc.cycles > 0 && tc.mode == "required" {
				want += tc.cycles
			}
			if tc.name == "review-missing-report" {
				// Even a premature scheduler completion cannot bypass required evidence.
				if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded' WHERE organization_id=$1 AND id=$2`, org, ids["review"]); err != nil {
					t.Fatal(err)
				}
				// Progress has already finalized this failed run. Inject the faulty
				// terminal state too, so this exercises the evidence guard itself.
				if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET state='succeeded' WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.PresentForReview(ctx, org, run.ID, candidate, strings.Repeat("e", 64), run.VerificationSHA256); !errors.Is(err, workflow.ErrEvidencePending) {
					t.Fatalf("missing review report bypassed acceptance: %v", err)
				}
			}
			if attempts != want {
				t.Fatalf("implementation attempts=%d, want %d", attempts, want)
			}
			if tc.name == "repair-policy" {
				checkpoint, err := store.RecordGoalCheckpoint(ctx, owner, launchInput.GoalID, run.ID, pkg.ID, "Greeting accepted", "checkpoint", 1)
				if err != nil {
					t.Fatal(err)
				}
				if commit, err := store.GoalCheckpointSource(ctx, org, launchInput.GoalID, checkpoint.ID, launchInput.SourceRepositoryURL); err != nil || commit != candidate {
					t.Fatalf("resolve continuation: %s %v", commit, err)
				}
				// Read the actual pushed candidate by exact SHA into a fresh checkout.
				checkout := t.TempDir()
				for _, args := range [][]string{{"init", "--template=", checkout}, {"-C", checkout, "fetch", "--depth=1", "--no-tags", remote, candidate}, {"-C", checkout, "checkout", "--detach", "FETCH_HEAD"}} {
					if out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput(); err != nil {
						t.Fatalf("continuation fetch: %s %v", out, err)
					}
				}
				if got, err := os.ReadFile(filepath.Join(checkout, "greeting.txt")); err != nil || string(got) != "hello\n" {
					t.Fatalf("continuation lost accepted work: %q %v", got, err)
				}
				continued := launchInput
				continued.LaunchKey, continued.CheckpointID = "continued", checkpoint.ID
				continued.Source.Repo, continued.Source.Ref, continued.Source.CheckpointID = checkout, candidate, checkpoint.ID
				preview, err := store.PreviewSource(ctx, owner, project, continued.Source)
				if err != nil {
					t.Fatal(err)
				}
				continued.ExpectedBundleSHA256 = preview.Digest
				continued.ExpectedVerificationSHA256, err = workflow.PreviewVerification(preview, policy)
				if err != nil {
					t.Fatal(err)
				}
				bad := continued
				bad.Source.Ref = run.SourceCommit
				bad.ExpectedBundleSHA256, bad.ExpectedVerificationSHA256 = "", ""
				bad.Source.Repo = repo
				if _, err := store.CreateFrozenRun(ctx, bad); !errors.Is(err, workflow.ErrConflict) {
					t.Fatalf("wrong checkpoint commit admitted: %v", err)
				}
				bad = continued
				bad.CheckpointID, bad.Source.CheckpointID = "", ""
				if _, err := store.CreateFrozenRun(ctx, bad); !errors.Is(err, workflow.ErrPreviewChanged) {
					t.Fatalf("preview did not bind checkpoint: %v", err)
				}
				if _, err := store.GoalCheckpointSource(ctx, org, project, checkpoint.ID, continued.SourceRepositoryURL); !errors.Is(err, workflow.ErrNotFound) {
					t.Fatalf("cross-goal checkpoint resolved: %v", err)
				}
				if _, err := store.GoalCheckpointSource(ctx, org, continued.GoalID, checkpoint.ID, "https://github.com/example/other"); !errors.Is(err, workflow.ErrConflict) {
					t.Fatalf("cross-repository checkpoint resolved: %v", err)
				}
				if _, err := store.SetProjectSourceAs(ctx, owner, project, continued.SourceRepositoryURL, "changed", ""); err != nil {
					t.Fatal(err)
				}
				if _, err := store.CreateFrozenRun(ctx, continued); !errors.Is(err, workflow.ErrConflict) {
					t.Fatalf("changed project ref admitted: %v", err)
				}
				if _, err := store.SetProjectSourceAs(ctx, owner, project, continued.SourceRepositoryURL, "main", ""); err != nil {
					t.Fatal(err)
				}
				parentRun := run
				run, err = store.CreateFrozenRun(ctx, continued)
				if err != nil {
					t.Fatal(err)
				}
				if replay, err := store.CreateFrozenRun(ctx, continued); err != nil || replay.ID != run.ID {
					t.Fatalf("continuation replay: %v", err)
				}
				if run.SourceCommit != checkpoint.CandidateRevision || run.ID == parentRun.ID {
					t.Fatal("continuation source/identity")
				}
				tasks, err = store.ListRunTasks(ctx, org, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, task := range tasks {
					ids[task.Key] = task.ID
				}
				frozen, err = store.LoadFrozenTask(ctx, org, run.ID, ids["implement"])
				if err != nil || frozen.SourceRef != candidate || frozen.Bundle.Source.CheckpointID != checkpoint.ID {
					t.Fatalf("worker continuation not pinned: %v", err)
				}
				interactions, _ := interact.New(pool)
				runs, err := interactions.GoalRuns(ctx, org, continued.GoalID)
				if err != nil || len(runs) != 2 || runs[0].CheckpointID != checkpoint.ID {
					t.Fatalf("continuation lineage: %+v %v", runs, err)
				}
				complete("implement", "hello")
				complete("verify", "")
				if _, err := store.Progress(ctx, "", "", 100, nil); err != nil {
					t.Fatal(err)
				}
				if accepted, err := store.GetCurrentReview(ctx, org, run.ID); err != nil || accepted.IntegratedCommit != candidate || accepted.ID == pkg.ID {
					t.Fatalf("continuation requires own acceptance: %+v %v", accepted, err)
				}
				if report, err := store.DeliveryReport(ctx, org, run.ID); err != nil || !strings.Contains(report.Markdown, checkpoint.ID) {
					t.Fatalf("continuation export: %v", err)
				}
				// A later parent package makes this source unavailable to new admissions.
				if _, err := store.PresentForReview(ctx, org, parentRun.ID, checkpoint.CandidateRevision, strings.Repeat("e", 64), parentRun.VerificationSHA256); err != nil {
					t.Fatal(err)
				}
				continued.LaunchKey = "superseded-continuation"
				if _, err := store.CreateFrozenRun(ctx, continued); !errors.Is(err, workflow.ErrConflict) {
					t.Fatalf("superseded checkpoint admitted: %v", err)
				}
				if _, err := store.GoalCheckpointSource(ctx, org, continued.GoalID, checkpoint.ID, continued.SourceRepositoryURL); !errors.Is(err, workflow.ErrConflict) {
					t.Fatalf("superseded checkpoint preview: %v", err)
				}
			}

		})
	}
}
