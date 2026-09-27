package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Invoked by the real TLS application test: no mock HTTP or identity/store.
func testMachineAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, browser *http.Client, origin, csrf, projectID, runID string, owner identity.FirstOwner) {
	t.Helper()
	web := apiv1connect.NewWorkflowServiceClient(browser, origin+"/api")
	var err error
	mint := func(scopes ...string) *api.CreateApiTokenResponse {
		t.Helper()
		r := connect.NewRequest(&api.CreateApiTokenRequest{ProjectId: projectID, Label: "External factory", Scopes: scopes, LifetimeSeconds: 3600})
		r.Header().Set("Origin", origin)
		if _, err := web.CreateApiToken(ctx, r); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("token without CSRF: %v", err)
		}
		r.Header().Set("X-Blaxsmith-CSRF", csrf)
		out, err := web.CreateApiToken(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		return out.Msg
	}
	machine := func(token string) apiv1connect.WorkflowServiceClient {
		return apiv1connect.NewWorkflowServiceClient(&http.Client{Transport: browser.Transport, Timeout: 5 * time.Second}, origin+"/machine", connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
				r.Header().Set("Authorization", "Bearer "+token)
				return next(ctx, r)
			}
		})))
	}
	invalidLifetime := connect.NewRequest(&api.CreateApiTokenRequest{ProjectId: projectID, Label: "Over limit", Scopes: []string{"project.read"}, LifetimeSeconds: 2147483647})
	invalidLifetime.Header().Set("Origin", origin)
	invalidLifetime.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := web.CreateApiToken(ctx, invalidLifetime); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("over-limit token lifetime accepted: %v", err)
	}
	token := mint("project.read", "goal.write")
	agent := machine(token.Token)

	// External factory API -> exact goal/plan binding -> real Git freeze and
	// PostgreSQL admission. No Anvil planner, schema or compiler is involved.
	external, err := agent.CreateGoal(ctx, connect.NewRequest(&api.CreateGoalRequest{ProjectId: projectID, RequestKey: "factory-binding", Title: "External plan", Brief: "External methodology", FactoryId: "example-factory", FactoryVersion: "1"}))
	if err != nil {
		t.Fatal(err)
	}
	externalJSON := `{"schema_version":"example-factory/plan/v1","tasks":[{"title":"Inspect"}],"large_integer":9007199254740993}`
	saved, err := agent.SaveGoalPlan(ctx, connect.NewRequest(&api.SaveGoalPlanRequest{GoalId: external.Msg.Goal.Id, RequestKey: "save-external", ExpectedGoalRevision: 1, ContentJson: externalJSON}))
	if err != nil {
		t.Fatal(err)
	}
	interactions, _ := interact.New(pool)
	store, _ := workflow.New(pool)
	service := workflowService{interactions: interactions}
	binding, factory, files, err := service.resolveFactoryGoal(ctx, identity.Caller{OrganizationID: owner.OrganizationID}, projectID, &api.GoalContext{GoalId: external.Msg.Goal.Id, ExpectedRevision: 1, PlanVersion: saved.Msg.Version})
	if err != nil || factory.ID != "example-factory" || !strings.Contains(string(files[".blaxsmith/platform/selected-plan.json"].Data), "9007199254740993") {
		t.Fatalf("factory context: %v", err)
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "prompt.md"), []byte("Inspect the bound factory context."), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--template=", "--initial-branch=main"}, {"add", "."}, {"commit", "-m", "fixture"}} {
		command := exec.CommandContext(ctx, "git", append([]string{"-C", repo, "-c", "user.name=E2E", "-c", "user.email=e2e@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("fixture Git: %v %s", err, output)
		}
	}
	body := []byte(`{"schema_version":"blaxsmith.recipe/v1alpha1","name":"external","factory":{"id":"example-factory","version":"1"},"acceptance":"manual","profiles":{"p":{"harness":"codex","model":"test","effort":"medium"}},"documents":[".blaxsmith/platform/goal.json",".blaxsmith/platform/selected-plan.json"],"stages":[{"id":"plan","kind":"plan","profile":"p","prompt":"prompt.md"}],"limits":{"max_correction_cycles":0,"timeout_seconds":60}}`)
	sourceReq := connect.NewRequest(&api.SetProjectSourceRequest{ProjectId: projectID, RepositoryUrl: "https://github.com/example/project.git", Ref: "main"})
	sourceReq.Header().Set("Origin", origin)
	sourceReq.Header().Set("X-Blaxsmith-CSRF", csrf)
	source, err := web.SetProjectSource(ctx, sourceReq)
	if err != nil {
		t.Fatalf("external source: %v", err)
	}
	policyReq := connect.NewRequest(&api.SetProjectVerificationRequest{ProjectId: projectID})
	policyReq.Header().Set("Origin", origin)
	policyReq.Header().Set("X-Blaxsmith-CSRF", csrf)
	policy, err := web.SetProjectVerification(ctx, policyReq)
	if err != nil {
		t.Fatalf("external verification: %v", err)
	}
	launchToken := mint("run.launch")
	expiry, err := time.Parse(time.RFC3339, launchToken.Credential.ExpiresAt)
	if err != nil {
		t.Fatal(err)
	}
	binding.Caller = &identity.Caller{OrganizationID: owner.OrganizationID, PrincipalID: owner.PrincipalID, SessionID: launchToken.Credential.Id, Role: "owner", AccessExpires: expiry}
	binding.SourceRepositoryURL = source.Msg.Source.RepositoryUrl
	binding.SourceRef = source.Msg.Source.Ref
	binding.VerificationVersion = policy.Msg.Verification.Version
	binding.OrganizationID = owner.OrganizationID
	binding.ProjectID = projectID
	binding.LaunchKey = "external-bound"
	binding.Verification = workflow.VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1"}
	binding.Source = recipe.Input{Repo: repo, Ref: "HEAD", Scope: ".", Recipe: ".blaxsmith/platform/external.json", RecipeData: body, PlatformFiles: files}
	allowanceRequest := connect.NewRequest(&api.SetGoalAllowanceRequest{GoalId: external.Msg.Goal.Id, MaxRuns: 2, MaxAttempts: 1})
	allowanceRequest.Header().Set("Origin", origin)
	allowanceRequest.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := web.SetGoalAllowance(ctx, allowanceRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.SetGoalAllowance(ctx, connect.NewRequest(&api.SetGoalAllowanceRequest{GoalId: external.Msg.Goal.Id, ExpectedVersion: 1, MaxRuns: 100})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("machine increased allowance: %v", err)
	}
	boundRun, err := store.CreateFrozenRun(ctx, binding)
	if err != nil {
		t.Fatalf("external frozen admission: %v", err)
	}
	association, err := agent.GetGoalPlans(ctx, connect.NewRequest(&api.GetGoalPlansRequest{GoalId: external.Msg.Goal.Id}))
	if err != nil || len(association.Msg.Runs) != 1 || association.Msg.Runs[0].Id != boundRun.ID || association.Msg.Runs[0].PlanVersion != 1 {
		t.Fatalf("external run association: %v", err)
	}
	// Matching launch retries remain idempotent even when the run cap is reached.
	if replay, err := store.CreateFrozenRun(ctx, binding); err != nil || replay.ID != boundRun.ID {
		t.Fatalf("allowance replay: %v", err)
	}
	controlAgent := machine(mint("run.control", "project.read").Token)
	pause := &api.ControlGoalRequest{GoalId: external.Msg.Goal.Id, Action: "pause", RequestKey: "pause-external"}
	if _, err := agent.ControlGoal(ctx, connect.NewRequest(pause)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("unscoped goal control: %v", err)
	}
	if _, err := controlAgent.ControlGoal(ctx, connect.NewRequest(pause)); err != nil {
		t.Fatal(err)
	}
	if replay, err := controlAgent.ControlGoal(ctx, connect.NewRequest(pause)); err != nil || replay.Msg.Version != 1 {
		t.Fatalf("control replay: %v", err)
	}
	firstPausedTasks, err := store.ListRunTasks(ctx, owner.OrganizationID, boundRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveAttempt(ctx, owner.OrganizationID, boundRun.ID, firstPausedTasks[0].ID); err != workflow.ErrGoalStopped {
		t.Fatalf("paused attempt admitted: %v", err)
	}
	if replay, err := store.CreateFrozenRun(ctx, binding); err != nil || replay.ID != boundRun.ID {
		t.Fatalf("paused launch receipt: %v", err)
	}
	binding.LaunchKey = "paused-new-run"
	if _, err := store.CreateFrozenRun(ctx, binding); err != workflow.ErrGoalStopped {
		t.Fatalf("paused run admitted: %v", err)
	}
	ready, err := store.ListReadyTasks(ctx, owner.OrganizationID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range ready {
		if task.RunID == boundRun.ID {
			t.Fatal("paused goal occupies scheduler page")
		}
	}
	// A new store/client observes durable pause without coordinator memory.
	replacement, _ := workflow.New(pool)
	if state, err := replacement.GetGoalControl(ctx, owner.OrganizationID, external.Msg.Goal.Id); err != nil || state.State != "paused" {
		t.Fatalf("pause lost on replacement: %+v %v", state, err)
	}
	resume := &api.ControlGoalRequest{GoalId: external.Msg.Goal.Id, Action: "resume", ExpectedVersion: 1, RequestKey: "resume-external"}
	if _, err := controlAgent.ControlGoal(ctx, connect.NewRequest(resume)); err != nil {
		t.Fatal(err)
	}
	stale := &api.ControlGoalRequest{GoalId: external.Msg.Goal.Id, Action: "pause", ExpectedVersion: 1, RequestKey: "stale-pause"}
	if _, err := controlAgent.ControlGoal(ctx, connect.NewRequest(stale)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale goal control: %v", err)
	}
	binding.LaunchKey = "external-second"
	secondRun, err := store.CreateFrozenRun(ctx, binding)
	if err != nil {
		t.Fatal(err)
	}
	if replay, err := store.CreateFrozenRun(ctx, binding); err != nil || replay.ID != secondRun.ID {
		t.Fatalf("at-cap replay: %v", err)
	}
	binding.LaunchKey = "external-over-limit"
	if _, err := store.CreateFrozenRun(ctx, binding); err != workflow.ErrGoalAllowance {
		t.Fatalf("run cap bypass: %v", err)
	}
	firstTasks, err := store.ListRunTasks(ctx, owner.OrganizationID, boundRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondTasks, err := store.ListRunTasks(ctx, owner.OrganizationID, secondRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	type admissionResult struct {
		attempt workflow.Attempt
		err     error
	}
	admissions := make(chan admissionResult, 2)
	start := make(chan struct{})
	for _, item := range []struct{ run, task string }{{boundRun.ID, firstTasks[0].ID}, {secondRun.ID, secondTasks[0].ID}} {
		go func() {
			<-start
			a, err := store.ReserveAttempt(ctx, owner.OrganizationID, item.run, item.task)
			admissions <- admissionResult{a, err}
		}()
	}
	close(start)
	allowed, denied := 0, 0
	var admitted workflow.Attempt
	for range 2 {
		result := <-admissions
		if result.err == nil {
			allowed++
			admitted = result.attempt
			if err := store.MarkUnknown(ctx, result.attempt); err != nil {
				t.Fatal(err)
			}
		} else if result.err == workflow.ErrGoalAllowance {
			denied++
		} else {
			t.Fatal(result.err)
		}
	}
	if allowed != 1 || denied != 1 {
		t.Fatalf("concurrent allowance admission: %d allowed %d denied", allowed, denied)
	}
	ready, err = store.ListReadyTasks(ctx, owner.OrganizationID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range ready {
		if task.RunID == boundRun.ID || task.RunID == secondRun.ID {
			t.Fatal("exhausted goal occupies scheduler page")
		}
	}
	usage, err := agent.GetGoalAllowance(ctx, connect.NewRequest(&api.GetGoalAllowanceRequest{GoalId: external.Msg.Goal.Id}))
	if err != nil || usage.Msg.Allowance.Runs != 2 || usage.Msg.Allowance.Attempts != 1 {
		t.Fatalf("allowance accounting: %+v %v", usage, err)
	}
	allowanceRequest.Msg.ExpectedVersion = 1
	allowanceRequest.Msg.MaxAttempts = 0
	allowanceRequest.Msg.AdmitUntil = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := web.SetGoalAllowance(ctx, allowanceRequest); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveAttempt(ctx, owner.OrganizationID, secondRun.ID, secondTasks[0].ID); err != workflow.ErrGoalAllowance {
		t.Fatalf("deadline bypassed: %v", err)
	}
	if _, err := web.SetGoalAllowance(ctx, allowanceRequest); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("stale allowance edit: %v", err)
	}
	var revisions int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_goal_allowance_history WHERE organization_id=$1 AND goal_id=$2`, owner.OrganizationID, external.Msg.Goal.Id).Scan(&revisions); err != nil || revisions != 2 {
		t.Fatalf("allowance history: %d %v", revisions, err)
	}
	cancelGoal := &api.ControlGoalRequest{GoalId: external.Msg.Goal.Id, Action: "cancel", ExpectedVersion: 2, RequestKey: "cancel-external"}
	if _, err := controlAgent.ControlGoal(ctx, connect.NewRequest(cancelGoal)); err != nil {
		t.Fatal(err)
	}
	if state, err := replacement.GetGoalControl(ctx, owner.OrganizationID, external.Msg.Goal.Id); err != nil || state.State != "cancel_requested" || state.StoppingRuns != 2 {
		t.Fatalf("cancel confirmation invented: %+v %v", state, err)
	}
	if err := store.FinalizeCancel(ctx, owner.OrganizationID, admitted.RunID); err != workflow.ErrConflict {
		t.Fatalf("active owner ignored: %v", err)
	}
	// Simulated connector stop evidence, not live AX qualification.
	if err := store.ConfirmStopped(ctx, admitted); err != nil {
		t.Fatal(err)
	}
	for _, run := range []string{boundRun.ID, secondRun.ID} {
		if err := store.FinalizeCancel(ctx, owner.OrganizationID, run); err != nil {
			t.Fatal(err)
		}
	}
	cancelledUsage := tooladapter.HarnessUsage("codex", []byte(`{"type":"turn.completed","usage":{"input_tokens":1500,"output_tokens":150}}`))
	if cancelledUsage == nil {
		t.Fatal("missing usage fixture")
	}
	if err := store.RecordUsage(ctx, admitted, *cancelledUsage); err != nil {
		t.Fatal(err)
	}
	if err := replacement.RecordUsage(ctx, admitted, *cancelledUsage); err != nil {
		t.Fatal(err)
	}
	if got, err := agent.GetGoalUsage(ctx, connect.NewRequest(&api.GetGoalUsageRequest{GoalId: external.Msg.Goal.Id})); err != nil || got.Msg.Usage.Reports != 1 || got.Msg.Usage.InputTokens != "1500" {
		t.Fatalf("cancelled goal lost/repeated usage: %v", err)
	}
	if state, err := replacement.GetGoalControl(ctx, owner.OrganizationID, external.Msg.Goal.Id); err != nil || state.State != "cancelled" {
		t.Fatalf("cancel not confirmed: %+v %v", state, err)
	}
	if _, err := controlAgent.ControlGoal(ctx, connect.NewRequest(cancelGoal)); err != nil {
		t.Fatalf("cancel replay: %v", err)
	}
	resume.ExpectedVersion = 3
	resume.RequestKey = "revive-cancelled"
	if _, err := controlAgent.ControlGoal(ctx, connect.NewRequest(resume)); connect.CodeOf(err) != connect.CodeAborted {
		t.Fatalf("cancelled goal resumed: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_goal_control_history WHERE organization_id=$1 AND goal_id=$2`, owner.OrganizationID, external.Msg.Goal.Id).Scan(&revisions); err != nil || revisions != 3 {
		t.Fatalf("goal control history: %d %v", revisions, err)
	}
	if _, _, _, err := service.resolveFactoryGoal(ctx, identity.Caller{OrganizationID: owner.OrganizationID}, projectID, &api.GoalContext{GoalId: external.Msg.Goal.Id, ExpectedRevision: 2, PlanVersion: 1}); err == nil {
		t.Fatal("stale factory context accepted")
	}
	if _, err := agent.ReplyGoal(ctx, connect.NewRequest(&api.ReplyGoalRequest{GoalId: external.Msg.Goal.Id, ExpectedRevision: 1, RequestKey: "change-external", Kind: "message", Text: "New scope"})); err != nil {
		t.Fatal(err)
	}
	binding.LaunchKey = "stale-external"
	if _, err := store.CreateFrozenRun(ctx, binding); err != workflow.ErrConflict {
		t.Fatalf("stale factory admission: %v", err)
	}
	request := connect.NewRequest(&api.CreateGoalRequest{ProjectId: projectID, RequestKey: "machine-goal", Title: "External client", Brief: "Preserve behavior."})
	goal, err := agent.CreateGoal(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := agent.CreateGoal(ctx, request)
	if err != nil || retry.Msg.Goal.Id != goal.Msg.Goal.Id {
		t.Fatalf("duplicate goal admission: %v", err)
	}
	request.Msg.Brief = "Different intent"
	if _, err = agent.CreateGoal(ctx, request); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("idempotency conflict: %v", err)
	}
	get := connect.NewRequest(&api.GetGoalRequest{GoalId: goal.Msg.Goal.Id})
	get.Header().Set("Origin", origin)
	observed, err := web.GetGoal(ctx, get)
	if err != nil || observed.Msg.Goal.Id != goal.Msg.Goal.Id {
		t.Fatalf("browser/machine read parity: %v", err)
	}
	get = connect.NewRequest(&api.GetGoalRequest{GoalId: goal.Msg.Goal.Id})
	if _, err = agent.GetGoal(ctx, get); err != nil {
		t.Fatal(err)
	}
	reply := connect.NewRequest(&api.ReplyGoalRequest{GoalId: goal.Msg.Goal.Id, ExpectedRevision: 1, RequestKey: "decision", Kind: "message", Text: "Use existing conventions."})
	if _, err = agent.ReplyGoal(ctx, reply); err != nil {
		t.Fatal(err)
	}
	reply.Msg.RequestKey = "stale"
	if _, err = agent.ReplyGoal(ctx, reply); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale revision accepted: %v", err)
	}
	var checkpointGoal string
	if err := pool.QueryRow(ctx, `SELECT goal_id FROM workflow_goal_checkpoints WHERE organization_id=$1 AND run_id=$2`, owner.OrganizationID, runID).Scan(&checkpointGoal); err != nil {
		t.Fatal(err)
	}
	if got, err := agent.ListGoalCheckpoints(ctx, connect.NewRequest(&api.ListGoalCheckpointsRequest{GoalId: checkpointGoal})); err != nil || len(got.Msg.Checkpoints) != 1 || got.Msg.Checkpoints[0].CurrentAcceptance {
		t.Fatalf("machine checkpoint history: %v", err)
	}
	reportedUsage, err := agent.GetRunUsage(ctx, connect.NewRequest(&api.GetRunUsageRequest{RunId: runID}))
	if err != nil || reportedUsage.Msg.Usage.InputTokens != "300" || reportedUsage.Msg.Usage.CostReports != 0 {
		t.Fatalf("machine usage: %v", err)
	}
	goalUsage, err := agent.GetGoalUsage(ctx, connect.NewRequest(&api.GetGoalUsageRequest{GoalId: goal.Msg.Goal.Id}))
	if err != nil || goalUsage.Msg.Usage.Reports != 0 {
		t.Fatalf("goal usage unknown: %v", err)
	}
	report, err := agent.GetDeliveryReport(ctx, connect.NewRequest(&api.GetDeliveryReportRequest{RunId: runID}))
	if err != nil || !strings.Contains(report.Msg.Markdown, "# Blaxsmith delivery snapshot") {
		t.Fatalf("machine delivery report: %v", err)
	}
	readonly := mint("project.read")
	// Exercise the actual CLI over stdio, with only API authority in its env.
	dir := t.TempDir()
	binary := filepath.Join(dir, "blaxsmith")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("MCP binary: %v %s", err, output)
	}
	contractCommand := exec.CommandContext(ctx, binary, "api-contract")
	contractCommand.Env = []string{}
	contractBytes, err := contractCommand.Output()
	if err != nil {
		t.Fatalf("offline API contract: %v", err)
	}
	reference, err := os.ReadFile("../../docs/machine-api-contract.json")
	if err != nil || string(reference) != string(contractBytes) {
		t.Fatal("machine API reference drift; run make api-reference")
	}
	var contract struct {
		Schema     string                           `json:"schema_version"`
		Operations []struct{ Method, Scope string } `json:"operations"`
	}
	if json.Unmarshal(contractBytes, &contract) != nil || contract.Schema != "blaxsmith.machine-contract/v1" || len(contract.Operations) != len(machineOperations) {
		t.Fatal("incomplete machine contract")
	}
	for _, operation := range contract.Operations {
		if access, ok := machineOperations[operation.Method]; !ok || access.scope != operation.Scope {
			t.Fatalf("reference scope mismatch: %s", operation.Method)
		}
	}
	probe, err := browser.Get(origin + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	probe.Body.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: probe.TLS.PeerCertificates[0].Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	// Execute the documented external factory example against this real TLS app.
	factoryBinary := filepath.Join(dir, "factory-client")
	factoryBuild := exec.CommandContext(ctx, "go", "build", "-o", factoryBinary, "../../examples/factory-client")
	if output, err := factoryBuild.CombinedOutput(); err != nil {
		t.Fatalf("factory quickstart build: %v %s", err, output)
	}
	planBytes, err := os.ReadFile("../../examples/factory-client/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(dir, "external-plan.json")
	if err := os.WriteFile(planPath, planBytes, 0600); err != nil {
		t.Fatal(err)
	}
	quickstart := func(credential string) ([]byte, error) {
		command := exec.CommandContext(ctx, factoryBinary, "--server", origin, "--request-key", "quickstart-e2e", "--plan", planPath)
		command.Env = []string{"PATH=" + os.Getenv("PATH"), "SSL_CERT_FILE=" + ca, "BLAXSMITH_API_TOKEN=" + credential}
		return command.Output()
	}
	if output, err := quickstart(readonly.Token); err == nil || len(output) != 0 {
		t.Fatalf("read-only quickstart mutated state: %v", err)
	}
	firstReceipt, err := quickstart(token.Token)
	if err != nil {
		t.Fatalf("factory quickstart: %v", err)
	}
	secondReceipt, err := quickstart(token.Token)
	if err != nil || string(firstReceipt) != string(secondReceipt) {
		t.Fatalf("quickstart replay changed durable IDs: %v", err)
	}
	var receipt struct {
		GoalID string `json:"goal_id"`
	}
	if err := json.NewDecoder(bytes.NewReader(firstReceipt)).Decode(&receipt); err != nil || receipt.GoalID == "" {
		t.Fatalf("quickstart receipt: %v", err)
	}
	if err := os.WriteFile(planPath, []byte(strings.Replace(string(planBytes), "Inspect the repository", "Inspect and document the repository", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := quickstart(token.Token); err == nil || !strings.Contains(string(output), receipt.GoalID) {
		t.Fatalf("changed quickstart replay succeeded or lost committed goal receipt: %v", err)
	}
	quickstartPlans, err := agent.GetGoalPlans(ctx, connect.NewRequest(&api.GetGoalPlansRequest{GoalId: receipt.GoalID}))
	if err != nil || len(quickstartPlans.Msg.Plans) != 1 || len(quickstartPlans.Msg.Runs) != 0 {
		t.Fatalf("quickstart duplicated plans or launched work: %v", err)
	}
	command := exec.CommandContext(ctx, binary, "mcp", "--server", origin)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "SSL_CERT_FILE=" + ca, "BLAXSMITH_API_TOKEN=" + token.Token}
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "blaxsmith-e2e", Version: "1"}, nil)
	mcpSession, err := mcpClient.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer mcpSession.Close()
	tools, err := mcpSession.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if strings.Contains(tool.Name, "LaunchRun") || strings.Contains(tool.Name, "DecideReview") {
			t.Fatalf("ungranted MCP tool listed: %s", tool.Name)
		}
	}
	usageTool, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "blaxsmith_GetRunUsage", Arguments: map[string]any{"runId": runID}})
	if err != nil || usageTool.IsError || !strings.Contains(usageTool.Content[0].(*mcp.TextContent).Text, `"inputTokens":"300"`) {
		t.Fatalf("MCP usage parity: %v %+v", err, usageTool)
	}
	remoteHTTP := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.Header.Set("Authorization", "Bearer "+token.Token)
		return browser.Transport.RoundTrip(copy)
	}), Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	remoteClient := mcp.NewClient(&mcp.Implementation{Name: "blaxsmith-http-e2e", Version: "1"}, nil)
	remote, err := remoteClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: origin + "/mcp", HTTPClient: remoteHTTP, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("remote MCP initialize: %v", err)
	}
	defer remote.Close()
	remoteTools, err := remote.ListTools(ctx, nil)
	if err != nil || len(remoteTools.Tools) != len(tools.Tools) {
		t.Fatalf("remote MCP tool parity: %v", err)
	}
	for _, tool := range remoteTools.Tools {
		if strings.Contains(tool.Name, "LaunchRun") || strings.Contains(tool.Name, "DecideReview") {
			t.Fatalf("remote MCP leaked tool %s", tool.Name)
		}
	}
	unauthenticated := &http.Client{Transport: browser.Transport, Timeout: 5 * time.Second}
	probeRequest, _ := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/mcp", strings.NewReader(`{}`))
	probeResponse, err := unauthenticated.Do(probeRequest)
	if err != nil {
		t.Fatal(err)
	}
	probeResponse.Body.Close()
	if probeResponse.StatusCode != http.StatusUnauthorized {
		t.Fatalf("MCP missing credential: %d", probeResponse.StatusCode)
	}
	probeRequest.Header.Set("Origin", "https://untrusted.invalid")
	probeResponse, err = remoteHTTP.Do(probeRequest)
	if err != nil {
		t.Fatal(err)
	}
	probeResponse.Body.Close()
	if probeResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("MCP untrusted Origin: %d", probeResponse.StatusCode)
	}
	mcpResult, err := mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "blaxsmith_CreateGoal", Arguments: map[string]any{"projectId": projectID, "requestKey": "mcp-e2e", "title": "MCP goal", "brief": "Use public APIs only."}})
	if err != nil || mcpResult.IsError {
		t.Fatalf("MCP create goal failed: %v %+v", err, mcpResult)
	}
	var mcpGoal struct {
		Goal struct {
			ID string `json:"id"`
		} `json:"goal"`
	}
	if err = json.Unmarshal([]byte(mcpResult.Content[0].(*mcp.TextContent).Text), &mcpGoal); err != nil || mcpGoal.Goal.ID == "" {
		t.Fatal("missing MCP goal identity")
	}
	remoteResult, err := remote.CallTool(ctx, &mcp.CallToolParams{Name: "blaxsmith_CreateGoal", Arguments: map[string]any{"projectId": projectID, "requestKey": "mcp-e2e", "title": "MCP goal", "brief": "Use public APIs only."}})
	if err != nil || remoteResult.IsError || remoteResult.Content[0].(*mcp.TextContent).Text != mcpResult.Content[0].(*mcp.TextContent).Text {
		t.Fatalf("MCP cross-transport idempotency: %v", err)
	}
	if _, err = agent.GetGoal(ctx, connect.NewRequest(&api.GetGoalRequest{GoalId: mcpGoal.Goal.ID})); err != nil {
		t.Fatalf("MCP/API state parity: %v", err)
	}
	if _, err = machine(readonly.Token).CreateGoal(ctx, request); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("read token wrote goal: %v", err)
	}
	if _, err = agent.GetProject(ctx, connect.NewRequest(&api.GetProjectRequest{ProjectId: "00000000-0000-0000-0000-000000000000"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("out-of-project read: %v", err)
	}
	if _, err = agent.PreviewRun(ctx, connect.NewRequest(&api.PreviewRunRequest{ProjectId: projectID})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("launch without scope: %v", err)
	}
	if _, err = agent.CreateApiToken(ctx, connect.NewRequest(&api.CreateApiTokenRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("machine minted credential: %v", err)
	}
	if _, err = machine("provider-secret").GetGoal(ctx, get); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("unrelated bearer accepted: %v", err)
	}
	get.Header().Set("Cookie", "__Host-blaxsmith_access=not-a-session")
	if _, err = agent.GetGoal(ctx, get); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("ambiguous cookie authority: %v", err)
	}
	get.Header().Del("Cookie")
	list := connect.NewRequest(&api.ListApiTokensRequest{ProjectId: projectID})
	list.Header().Set("Origin", origin)
	inventory, err := web.ListApiTokens(ctx, list)
	if err != nil || len(inventory.Msg.Credentials) != 4 {
		t.Fatalf("credential inventory: %v", err)
	}
	revoke := connect.NewRequest(&api.RevokeApiTokenRequest{TokenId: token.Credential.Id})
	revoke.Header().Set("Origin", origin)
	revoke.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err = web.RevokeApiToken(ctx, revoke); err != nil {
		t.Fatal(err)
	}
	if _, err = web.RevokeApiToken(ctx, revoke); err != nil {
		t.Fatal(err)
	}
	if _, err = agent.GetGoal(ctx, get); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked token still valid: %v", err)
	}
	mcpResult, err = mcpSession.CallTool(ctx, &mcp.CallToolParams{Name: "blaxsmith_GetGoal", Arguments: map[string]any{"goalId": mcpGoal.Goal.ID}})
	if err != nil || !mcpResult.IsError {
		t.Fatalf("connected MCP client retained revoked authority: %v", err)
	}
	if _, err := remote.CallTool(ctx, &mcp.CallToolParams{Name: "blaxsmith_GetGoal", Arguments: map[string]any{"goalId": mcpGoal.Goal.ID}}); err == nil {
		t.Fatal("remote MCP retained revoked authority")
	}
	if _, err = pool.Exec(ctx, `UPDATE identity_sessions SET expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, readonly.Credential.Id); err != nil {
		t.Fatal(err)
	}
	if _, err = machine(readonly.Token).GetGoal(ctx, get); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expired token still valid: %v", err)
	}
	testServiceIdentityAPI(t, ctx, pool, web, machine, origin, csrf, projectID, owner)
	var plaintext int
	if err = pool.QueryRow(ctx, `SELECT count(*) FROM identity_api_tokens WHERE token_hash=convert_to($1,'UTF8')`, token.Token).Scan(&plaintext); err != nil || plaintext != 0 {
		t.Fatalf("plaintext credential storage: %v", err)
	}
}

// A service identity rotates credentials without becoming its administrator.
func testServiceIdentityAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, web apiv1connect.WorkflowServiceClient, machine func(string) apiv1connect.WorkflowServiceClient, origin, csrf, project string, owner identity.FirstOwner) {
	t.Helper()
	headers := func(r connect.AnyRequest) { r.Header().Set("Origin", origin); r.Header().Set("X-Blaxsmith-CSRF", csrf) }
	create := connect.NewRequest(&api.CreateServicePrincipalRequest{ProjectId: project, Label: "Unattended factory", RequestKey: "service-factory"})
	headers(create)
	principal, err := web.CreateServicePrincipal(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := web.CreateServicePrincipal(ctx, create)
	if err != nil || replay.Msg.Principal.Id != principal.Msg.Principal.Id {
		t.Fatalf("service identity retry: %v", err)
	}
	create.Msg.Label = "Changed payload"
	if _, err := web.CreateServicePrincipal(ctx, create); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("service identity key reused: %v", err)
	}
	issue := connect.NewRequest(&api.CreateApiTokenRequest{ProjectId: project, ServicePrincipalId: principal.Msg.Principal.Id, Label: "Automation credential", Scopes: []string{"project.read", "goal.write", "run.control"}, LifetimeSeconds: 3600})
	headers(issue)
	first, err := web.CreateApiToken(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	second, err := web.CreateApiToken(ctx, issue)
	if err != nil {
		t.Fatal(err)
	}
	if first.Msg.Credential.PrincipalId != principal.Msg.Principal.Id || first.Msg.Credential.Kind != "service" || first.Msg.Credential.PrincipalId == owner.PrincipalID {
		t.Fatal("service token impersonates issuer")
	}
	issue.Msg.Scopes = []string{"review.decide"}
	if _, err := web.CreateApiToken(ctx, issue); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("service received human review authority: %v", err)
	}
	issue.Msg.Scopes = []string{"project.read"}
	issue.Msg.ProjectId = "00000000-0000-0000-0000-000000000000"
	if _, err := web.CreateApiToken(ctx, issue); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("service moved projects: %v", err)
	}
	bot := machine(first.Msg.Token)
	replacement := machine(second.Msg.Token)
	if _, err := bot.CreateServicePrincipal(ctx, connect.NewRequest(&api.CreateServicePrincipalRequest{})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("service created service identity: %v", err)
	}
	if _, err := bot.CreateApiToken(ctx, connect.NewRequest(issue.Msg)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("service minted credential: %v", err)
	}
	if _, err := bot.GetProject(ctx, connect.NewRequest(&api.GetProjectRequest{ProjectId: "00000000-0000-0000-0000-000000000000"})); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("service escaped project: %v", err)
	}
	// Human MFA is not fabricated on unattended service sessions. Service work
	// remains valid when the organization enables MFA for its human identities.
	var oldMFA string
	if err := pool.QueryRow(ctx, `SELECT mfa_policy FROM identity_organizations WHERE id=$1`, owner.OrganizationID).Scan(&oldMFA); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET mfa_policy='required' WHERE id=$1`, owner.OrganizationID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET mfa_policy=$2 WHERE id=$1`, owner.OrganizationID, oldMFA); err != nil {
			t.Error(err)
		}
	}()
	goal, err := bot.CreateGoal(ctx, connect.NewRequest(&api.CreateGoalRequest{ProjectId: project, Title: "Service work", Brief: "Preserve service attribution", RequestKey: "service-goal"}))
	if err != nil {
		t.Fatal(err)
	}
	if goal.Msg.Goal.CreatedBy != principal.Msg.Principal.Id {
		t.Fatal("service goal attributed to human issuer")
	}
	if _, err := replacement.ControlGoal(ctx, connect.NewRequest(&api.ControlGoalRequest{GoalId: goal.Msg.Goal.Id, Action: "pause", RequestKey: "service-pause"})); err != nil {
		t.Fatalf("rotation lost service control: %v", err)
	}
	var method, mfa string
	if err := pool.QueryRow(ctx, `SELECT auth_method,mfa_level FROM identity_sessions WHERE id=$1`, first.Msg.Credential.Id).Scan(&method, &mfa); err != nil || method != "service" || mfa != "none" {
		t.Fatalf("service authentication attribution: %s %s %v", method, mfa, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_principals SET password_hash='not-a-password' WHERE id=$1`, principal.Msg.Principal.Id); err == nil {
		t.Fatal("service acquired password login")
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET mfa_policy=$2 WHERE id=$1`, owner.OrganizationID, oldMFA); err != nil {
		t.Fatal(err)
	}
	revoke := connect.NewRequest(&api.RevokeApiTokenRequest{TokenId: first.Msg.Credential.Id})
	headers(revoke)
	if _, err := web.RevokeApiToken(ctx, revoke); err != nil {
		t.Fatal(err)
	}
	if _, err := bot.GetGoal(ctx, connect.NewRequest(&api.GetGoalRequest{GoalId: goal.Msg.Goal.Id})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked service credential live: %v", err)
	}
	if _, err := replacement.GetGoal(ctx, connect.NewRequest(&api.GetGoalRequest{GoalId: goal.Msg.Goal.Id})); err != nil {
		t.Fatalf("replacement credential revoked: %v", err)
	}
	list := connect.NewRequest(&api.ListApiTokensRequest{ProjectId: project, ServicePrincipalId: principal.Msg.Principal.Id})
	headers(list)
	inventory, err := web.ListApiTokens(ctx, list)
	if err != nil || len(inventory.Msg.Credentials) != 2 {
		t.Fatalf("service token inventory: %v", err)
	}
	disable := connect.NewRequest(&api.DisableServicePrincipalRequest{PrincipalId: principal.Msg.Principal.Id})
	headers(disable)
	if _, err := web.DisableServicePrincipal(ctx, disable); err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.GetGoal(ctx, connect.NewRequest(&api.GetGoalRequest{GoalId: goal.Msg.Goal.Id})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("disabled service still authorized: %v", err)
	}
	issue.Msg.ProjectId = project
	if _, err := web.CreateApiToken(ctx, issue); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("disabled service acquired credential: %v", err)
	}
}
