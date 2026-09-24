package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type dispatchAX struct {
	task                       *axbridge.Task
	workspace                  axbridge.Workspace
	attemptWorkspaces          map[string]axbridge.Workspace
	gateway                    axbridge.Gateway
	attemptGateways            map[string]axbridge.Gateway
	invalidateGatewayAfterTask bool
	delayWorkspaceReady        bool
	workspaceReadyAt           time.Time
}

func (a *dispatchAX) Get(context.Context, string, string) (axbridge.Task, error) {
	if a.task == nil {
		return axbridge.Task{}, axbridge.ErrNotFound
	}
	if !a.workspaceReadyAt.IsZero() && !time.Now().Before(a.workspaceReadyAt) {
		a.task.Status.Conditions = []axbridge.TaskCondition{{Type: "WorkspaceReady", Status: "True", Reason: "SetupComplete"}}
		a.workspaceReadyAt = time.Time{}
	}
	return *a.task, nil
}
func (a *dispatchAX) Apply(_ context.Context, task axbridge.Task) error {
	refs, _ := task.Spec["workspaces"].([]any)
	for _, item := range refs {
		ref, _ := item.(map[string]any)
		name, _ := ref["name"].(string)
		if name != "source" {
			if _, ok := a.attemptWorkspaces[name]; !ok {
				return axbridge.ErrInputs
			}
		}
	}
	if ref, ok := task.Spec["gateway"].(map[string]any); ok {
		name, _ := ref["name"].(string)
		if _, ok := a.attemptGateways[name]; !ok {
			return axbridge.ErrInputs
		}
	}
	task.Status.Phase, task.Status.Actor = "Running", task.Metadata.Name
	readyStatus, readyReason := "True", "SetupComplete"
	if a.delayWorkspaceReady {
		readyStatus, readyReason = "False", "Initializing"
	}
	task.Status.Conditions = []axbridge.TaskCondition{{Type: "WorkspaceReady", Status: readyStatus, Reason: readyReason}}
	a.task = &task
	return nil
}
func (a *dispatchAX) Delete(context.Context, string, string) error { a.task = nil; return nil }
func (a *dispatchAX) GetWorkspace(_ context.Context, _, name string) (axbridge.Workspace, error) {
	if name == a.workspace.Metadata.Name {
		return a.workspace, nil
	}
	workspace, ok := a.attemptWorkspaces[name]
	if !ok {
		return axbridge.Workspace{}, axbridge.ErrNotFound
	}
	return workspace, nil
}
func (a *dispatchAX) ApplyWorkspace(_ context.Context, workspace axbridge.Workspace) error {
	if a.attemptWorkspaces == nil {
		a.attemptWorkspaces = make(map[string]axbridge.Workspace)
	}
	a.attemptWorkspaces[workspace.Metadata.Name] = workspace
	return nil
}
func (a *dispatchAX) DeleteWorkspace(_ context.Context, _, name string) error {
	delete(a.attemptWorkspaces, name)
	return nil
}
func (a *dispatchAX) GetGateway(_ context.Context, _, name string) (axbridge.Gateway, error) {
	if name == a.gateway.Metadata.Name {
		return a.gateway, nil
	}
	gateway, ok := a.attemptGateways[name]
	if !ok {
		return axbridge.Gateway{}, axbridge.ErrNotFound
	}
	if a.invalidateGatewayAfterTask && a.task != nil {
		return axbridge.Gateway{}, nil
	}
	return gateway, nil
}
func (a *dispatchAX) ApplyGateway(_ context.Context, gateway axbridge.Gateway) error {
	if a.attemptGateways == nil {
		a.attemptGateways = make(map[string]axbridge.Gateway)
	}
	a.attemptGateways[gateway.Metadata.Name] = gateway
	return nil
}
func (a *dispatchAX) DeleteGateway(_ context.Context, _, name string) error {
	delete(a.attemptGateways, name)
	return nil
}

type dispatchActor struct{ image, pool, signer, storage string }

func (a dispatchActor) Current(_ context.Context, space, name string) (bootstrap.Runtime, error) {
	return bootstrap.Runtime{Actor: bootstrap.Actor{Atespace: space, Name: name, UID: "actor-uid"},
		TemplateUID: "template-uid", Image: a.image, WorkerPool: a.pool, WorkerPod: "pod", WorkerPodUID: "pod-uid",
		SandboxClass: "SANDBOX_CLASS_GVISOR", BootstrapPublicKey: a.signer,
		SnapshotOnPause: "SNAPSHOT_CONTENT_SCOPE_DATA", SnapshotOnCommit: "SNAPSHOT_CONTENT_SCOPE_DATA",
		ResumeFromData: "RESUME_SOURCE_GOLDEN", SnapshotStorage: a.storage}, nil
}
func (dispatchActor) Gone(context.Context, string, string) (bool, error) { return true, nil }

func TestDispatchBatchPostgres(t *testing.T) {
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	ctx := t.Context()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_dispatch_" + hex.EncodeToString(suffix[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Errorf("drop temporary schema: %v", err)
		}
	}()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	var orgID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'dispatch-test','Dispatch Test') RETURNING id`).Scan(&orgID); err != nil {
		t.Fatal(err)
	}
	projectID, err := store.CreateProject(ctx, orgID, "dispatch-project", "Dispatch project")
	if err != nil {
		t.Fatal(err)
	}
	image := "runner@sha256:" + strings.Repeat("a", 64)
	artifact := func(name, body string) recipe.Artifact {
		return recipe.Artifact{Path: name, Data: []byte(body), SHA256: dispatchSHA([]byte(body))}
	}
	bundle := recipe.Bundle{SchemaVersion: "blaxsmith.bundle/v1alpha1",
		Source: recipe.Source{Commit: strings.Repeat("b", 40), Spec: "spec.md", Transcript: "transcript.md", Scope: "."},
		Recipe: recipe.Recipe{Profiles: map[string]recipe.Profile{"engineer": {Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"}},
			Stages:         []recipe.Stage{{ID: "plan", Kind: "plan", Profile: "engineer", Prompt: "plan.md"}},
			RequiredChecks: []string{"checks"}, Limits: recipe.Limits{MaxCorrectionCycles: 1, TimeoutSeconds: 60}},
		StageOrder: []string{"plan"}, Artifacts: []recipe.Artifact{artifact("spec.md", "requirements"),
			artifact("transcript.md", "decisions"), artifact("plan.md", "make a plan")}}
	canonical, _ := json.Marshal(bundle)
	bundle.Digest = dispatchSHA(canonical)
	bundleJSON, _ := json.Marshal(bundle)
	policy := workflow.VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1",
		Checks: []workflow.VerificationCheck{{ID: "checks", Command: []string{"true"}}}}
	policyJSON, _ := json.Marshal(policy)
	policySHA := dispatchSHA(policyJSON)
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID, LaunchKey: "launch",
		SourceCommit: bundle.Source.Commit, BundleSHA256: bundle.Digest, VerificationSHA256: policySHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref)
		VALUES ($1,$2,$3,$4,'https://github.com/owner/repo','main')`, orgID, run.ID, bundleJSON, policyJSON); err != nil {
		t.Fatal(err)
	}
	inputSHA := dispatchSHA([]byte(bundle.Digest + ":" + policySHA + ":plan"))
	if _, err := store.AddTask(ctx, orgID, run.ID, "plan", inputSHA, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, orgID, run.ID); err != nil {
		t.Fatal(err)
	}
	for i, statement := range []string{
		`INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		 VALUES ($1,'openai','openai','https://api.openai.com',ARRAY['native_raw'],'active')`,
		`INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		 VALUES ($1,'connection','organization',$1,'openai','account','api_key','active')`,
		`INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
		 VALUES ($1,$2,1,false,ARRAY['native_raw'])`,
		`INSERT INTO access_grants (organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		 VALUES ($1,'grant','connection',$2,'workload','blaxsmith-dispatcher','model.invoke','openai/gpt-6-luna','native_raw','operator')`,
	} {
		args := []any{orgID}
		if i >= 2 {
			args = append(args, projectID)
		}
		if _, err := pool.Exec(ctx, statement, args...); err != nil {
			t.Fatal(err)
		}
	}
	secretStore, err := access.NewSecretStore(pool, "key", map[string][]byte{"key": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secretStore.Rotate(ctx, orgID, "connection", 0, []byte("private-test-key"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_tool_runtime_approvals
		(organization_id,harness,model,effort,image,binary_path,binary_sha256,version,worker_pool,
		 max_timeout_seconds,max_output_bytes,approved_by)
		VALUES ($1,'codex','gpt-6-luna','xhigh',$2,'/opt/blaxsmith/bin/codex',$3,'0.156.1','pool-a',60,1048576,'operator')`,
		orgID, image, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_project_model_grants
		(organization_id,project_id,provider,model,grant_id,grantee_id,approved_by)
		VALUES ($1,$2,'openai','gpt-6-luna','grant','blaxsmith-dispatcher','operator')`, orgID, projectID); err != nil {
		t.Fatal(err)
	}
	ax := &dispatchAX{}
	bridge := &axbridge.Bridge{AX: ax, Actor: dispatchActor{image, "pool-a", "signer", "gs://snapshots/test/"},
		Signer: "signer", Storage: "gs://snapshots/test/", Workspace: "source", Gateway: "public-egress",
		LookupIPv4: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "github.com" {
				return []netip.Addr{netip.MustParseAddr("140.82.114.3")}, nil
			}
			return []netip.Addr{netip.MustParseAddr("104.18.33.45")}, nil
		}}
	ownerRevocations := 0
	bridge.RevokeOwner = func(context.Context, workflow.Attempt) error { ownerRevocations++; return nil }
	if _, err := (&Dispatcher{Workflow: store, DB: pool, Secrets: secretStore, Bridge: bridge}).DispatchBatch(ctx, "", 1, 1); err != ErrNotReady {
		t.Fatalf("missing worker route preflight must block dispatch: %v", err)
	}
	var unreserved int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE organization_id=$1 AND run_id=$2`,
		orgID, run.ID).Scan(&unreserved); err != nil || unreserved != 0 {
		t.Fatalf("missing route reserved attempt: %d, %v", unreserved, err)
	}
	activationPreflights := 0
	dispatcher := Dispatcher{Workflow: store, DB: pool, Secrets: secretStore, Bridge: bridge,
		PreflightWorker: func(_ context.Context, approval workflow.ApprovedToolRuntime, sourceURL, provider string) error {
			if approval.WorkerPool != "pool-a" || sourceURL != "https://github.com/owner/repo" || provider != "openai" {
				t.Fatal("unexpected worker route preflight input")
			}
			return nil
		},
		PreflightActivation: func(context.Context) error { activationPreflights++; return nil },
		Activate: func(_ context.Context, attempt workflow.Attempt, runtime bootstrap.Runtime, binding workflow.RuntimeBinding, invoke access.ModelInvoke) error {
			if attempt.ID == "" || runtime.Actor.UID != "actor-uid" || invoke.AttemptID != attempt.ID ||
				binding.ActorUID != runtime.Actor.UID || binding.CommandSHA256 == "" || invoke.BindingID == "" ||
				invoke.Provider != "openai" || invoke.Model != "gpt-6-luna" || invoke.PolicyVersion < 1 {
				t.Fatal("unexpected model lease activation input")
			}
			return nil
		},
		ReleaseModel: func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error {
			return nil
		}}
	withoutActivation := dispatcher
	withoutActivation.Activate = nil
	if _, err := withoutActivation.DispatchBatch(ctx, "", 1, 1); err != ErrNotReady {
		t.Fatalf("missing bootstrap activator must block dispatch: %v", err)
	}
	withoutModelRelease := dispatcher
	withoutModelRelease.ReleaseModel = nil
	if _, err := withoutModelRelease.DispatchBatch(ctx, "", 1, 1); err != ErrNotReady {
		t.Fatalf("missing post-readiness model release must block dispatch: %v", err)
	}
	blocked, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(blocked.Outcomes) != 1 || blocked.Outcomes[0].State != "blocked" ||
		blocked.Outcomes[0].Err != axbridge.ErrInputs {
		t.Fatalf("missing native AX resources were admitted: %+v, %v", blocked, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_attempts WHERE organization_id=$1 AND run_id=$2`,
		orgID, run.ID).Scan(&unreserved); err != nil || unreserved != 0 {
		t.Fatalf("missing native AX resources reserved attempt: %d, %v", unreserved, err)
	}
	ax.workspace = axbridge.Workspace{APIVersion: "ax.io/v1alpha1", Kind: "Workspace",
		Metadata: axbridge.TaskMetadata{Name: "source", Atespace: axbridge.Space(orgID)}}
	ax.gateway = axbridge.Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway",
		Metadata: axbridge.TaskMetadata{Name: "public-egress", Atespace: axbridge.Space(orgID)}}
	ax.gateway.Spec.Egress = &axbridge.GatewayEgress{Allowlist: &axbridge.GatewayAllowlist{
		Hosts: []axbridge.GatewayHostRule{{Host: "140.82.114.3/32"}, {Host: "104.18.33.45/32"}}}}
	ax.delayWorkspaceReady = true
	activationSawWorkspacePending := false
	modelReleaseSawWorkspaceReady := false
	dispatcher.Activate = func(ctx context.Context, attempt workflow.Attempt, runtime bootstrap.Runtime, binding workflow.RuntimeBinding, invoke access.ModelInvoke) error {
		_, state, _, stateErr := store.CurrentAttempt(ctx, attempt)
		var startedEvents int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_events WHERE organization_id=$1 AND run_id=$2 AND attempt_id=$3 AND kind='attempt.started'`,
			orgID, attempt.RunID, attempt.ID).Scan(&startedEvents); err != nil {
			return err
		}
		if stateErr != nil || state != "starting" || startedEvents != 0 {
			return errors.New("workflow published started before AX Workspace readiness")
		}
		if ax.task == nil || ax.task.Status.Phase != "Running" || ax.task.Status.Actor != ax.task.Metadata.Name ||
			ax.task.Status.Conditions[0].Status != "False" || ax.task.Status.Conditions[0].Reason != "Initializing" {
			return errors.New("activation ran before checking the expected pending workspace")
		}
		activationSawWorkspacePending = true
		ax.workspaceReadyAt = time.Now().Add(25 * time.Millisecond)
		if attempt.ID == "" || runtime.Actor.UID != "actor-uid" || invoke.AttemptID != attempt.ID ||
			binding.ActorUID != runtime.Actor.UID || binding.CommandSHA256 == "" || invoke.BindingID == "" ||
			invoke.Provider != "openai" || invoke.Model != "gpt-6-luna" || invoke.PolicyVersion < 1 {
			return errors.New("unexpected model lease activation input")
		}
		return nil
	}
	dispatcher.ReleaseModel = func(ctx context.Context, attempt workflow.Attempt, _ bootstrap.Runtime,
		_ workflow.RuntimeBinding, _ access.ModelInvoke) error {
		_, state, _, stateErr := store.CurrentAttempt(ctx, attempt)
		if stateErr != nil || state != "running" || ax.task == nil ||
			ax.task.Status.Conditions[0].Status != "True" || ax.task.Status.Conditions[0].Reason != "SetupComplete" {
			return errors.New("model credential release ran before workspace readiness")
		}
		modelReleaseSawWorkspaceReady = true
		return nil
	}
	batch, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(batch.Outcomes) != 1 || batch.Outcomes[0].State != "started" ||
		batch.Outcomes[0].AttemptID == "" || batch.Outcomes[0].BindingID == "" || ax.task == nil || activationPreflights != 1 ||
		!activationSawWorkspacePending || !modelReleaseSawWorkspaceReady ||
		ax.task.Status.Conditions[0].Status != "True" || ax.task.Status.Conditions[0].Reason != "SetupComplete" {
		t.Fatalf("dispatch batch: %+v, %v", batch, err)
	}
	// A public source binds no Git capability; setup releases no Git lease.
	if _, _, private, err := access.AttemptGitRead(ctx, pool, orgID, batch.Outcomes[0].AttemptID); err != nil || private {
		t.Fatalf("public source bound git.read: %v %v", private, err)
	}
	workspaceName := axbridge.AttemptWorkspaceName(batch.Outcomes[0].AttemptID)
	workspace, ok := ax.attemptWorkspaces[workspaceName]
	gatewayName := axbridge.AttemptGatewayName(batch.Outcomes[0].AttemptID)
	attemptGateway, gatewayOK := ax.attemptGateways[gatewayName]
	command := ax.task.Spec["command"].([]any)[1].(string)
	if !ok || workspace.Metadata.Atespace != axbridge.Space(orgID) ||
		!strings.Contains(command, `"source_directory":"source"`) ||
		ax.task.Spec["workspaces"].([]any)[0].(map[string]any)["name"] != workspaceName || !gatewayOK ||
		attemptGateway.Metadata.Atespace != axbridge.Space(orgID) ||
		ax.task.Spec["gateway"].(map[string]any)["name"] != gatewayName {
		t.Fatalf("attempt inputs were not frozen into its AX task: workspace=%+v gateway=%+v task=%+v", workspace, attemptGateway, ax.task)
	}
	git := workspace.Spec["git"].([]any)[0].(map[string]any)
	if git["repo"] != "https://github.com/owner/repo" || git["branch"] != bundle.Source.Commit || git["dir"] != "source" {
		t.Fatalf("AX Workspace did not contain the frozen source: %+v", git)
	}
	if got := attemptGateway.Spec.Egress.Allowlist.Hosts; len(got) != 2 ||
		got[0].Host != "140.82.114.3/32" || got[1].Host != "104.18.33.45/32" {
		t.Fatalf("attempt Gateway did not copy the approved narrow routes: %+v", got)
	}
	var taskState, attemptState string
	if err := pool.QueryRow(ctx, `SELECT t.state,a.state FROM workflow_tasks t
		JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.id=t.active_attempt_id
		WHERE t.organization_id=$1 AND t.run_id=$2`, orgID, run.ID).Scan(&taskState, &attemptState); err != nil ||
		taskState != "running" || attemptState != "running" {
		t.Fatalf("tool exit inferred task success: %s/%s, %v", taskState, attemptState, err)
	}
	var startedEvents int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_events WHERE organization_id=$1 AND run_id=$2 AND attempt_id=$3 AND kind='attempt.started'`,
		orgID, run.ID, batch.Outcomes[0].AttemptID).Scan(&startedEvents); err != nil || startedEvents != 1 {
		t.Fatalf("Workspace readiness did not publish exactly one started event: %d %v", startedEvents, err)
	}
	// Completion rebuilds the launched command from durable inputs alone.
	running, err := store.ListRunningAttempts(ctx, "", "", 1)
	if err != nil || len(running) != 1 {
		t.Fatalf("running attempt: %+v, %v", running, err)
	}
	rebuilt, err := dispatcher.AttemptBridge(ctx, running[0])
	if err != nil || rebuilt.Workspace != workspaceName || rebuilt.Gateway != gatewayName || rebuilt.Tool.AttemptID != running[0].ID {
		t.Fatalf("attempt bridge: %+v, %v", rebuilt, err)
	}
	if rebuiltCommand, err := tooladapter.Command(*rebuilt.Tool); err != nil || rebuiltCommand[1] != command {
		t.Fatalf("completion would not match the launched command: %v", err)
	}
	secondRun, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID,
		LaunchKey: "activation-fails", SourceCommit: bundle.Source.Commit,
		BundleSHA256: bundle.Digest, VerificationSHA256: policySHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref)
		VALUES ($1,$2,$3,$4,'https://github.com/owner/repo','main')`, orgID, secondRun.ID, bundleJSON, policyJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTask(ctx, orgID, secondRun.ID, "plan", inputSHA, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, orgID, secondRun.ID); err != nil {
		t.Fatal(err)
	}
	ax.task = nil
	dispatcher.Activate = func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error {
		return errors.New("bootstrap release outcome uncertain")
	}
	unresolved, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(unresolved.Outcomes) != 1 || unresolved.Outcomes[0].State != "unresolved" || activationPreflights != 2 {
		t.Fatalf("activation failure was reported as started: %+v, %v", unresolved, err)
	}
	if err := pool.QueryRow(ctx, `SELECT t.state,a.state FROM workflow_tasks t
		JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.id=t.active_attempt_id
		WHERE t.organization_id=$1 AND t.run_id=$2`, orgID, secondRun.ID).Scan(&taskState, &attemptState); err != nil ||
		taskState != "reconciling" || attemptState != "reconciling" {
		t.Fatalf("activation failure did not fence the actor: %s/%s, %v", taskState, attemptState, err)
	}
	thirdRun, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID,
		LaunchKey: "gateway-changed", SourceCommit: bundle.Source.Commit,
		BundleSHA256: bundle.Digest, VerificationSHA256: policySHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref)
		VALUES ($1,$2,$3,$4,'https://github.com/owner/repo','main')`, orgID, thirdRun.ID, bundleJSON, policyJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTask(ctx, orgID, thirdRun.ID, "plan", inputSHA, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, orgID, thirdRun.ID); err != nil {
		t.Fatal(err)
	}
	ax.task = nil
	ax.invalidateGatewayAfterTask = true
	activationCalls := 0
	dispatcher.Activate = func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error {
		activationCalls++
		return nil
	}
	changed, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(changed.Outcomes) != 1 || changed.Outcomes[0].State != "unresolved" ||
		!errors.Is(changed.Outcomes[0].Err, axbridge.ErrInputs) || activationCalls != 0 {
		t.Fatalf("changed Gateway reached credential release: %+v, calls=%d, err=%v", changed, activationCalls, err)
	}
	if err := pool.QueryRow(ctx, `SELECT t.state,a.state FROM workflow_tasks t
		JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.id=t.active_attempt_id
		WHERE t.organization_id=$1 AND t.run_id=$2`, orgID, thirdRun.ID).Scan(&taskState, &attemptState); err != nil ||
		taskState != "reconciling" || attemptState != "reconciling" {
		t.Fatalf("changed Gateway did not fence the blocked actor: %s/%s, %v", taskState, attemptState, err)
	}
	fourthRun, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID,
		LaunchKey: "workspace-setup-fails", SourceCommit: bundle.Source.Commit,
		BundleSHA256: bundle.Digest, VerificationSHA256: policySHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref)
		VALUES ($1,$2,$3,$4,'https://github.com/owner/repo','main')`, orgID, fourthRun.ID, bundleJSON, policyJSON); err != nil {
		t.Fatal(err)
	}
	_, err = store.AddTask(ctx, orgID, fourthRun.ID, "plan", inputSHA, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, orgID, fourthRun.ID); err != nil {
		t.Fatal(err)
	}
	ax.task = nil
	ax.invalidateGatewayAfterTask = false
	dispatcher.Activate = func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error {
		ax.task.Status.Phase = "Failed"
		ax.task.Status.Conditions = []axbridge.TaskCondition{{Type: "WorkspaceReady", Status: "False", Reason: "Failed"}}
		return nil
	}
	failedSetup, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(failedSetup.Outcomes) != 1 || failedSetup.Outcomes[0].State != "stopped" ||
		!errors.Is(failedSetup.Outcomes[0].Err, axbridge.ErrWorkspaceSetup) || ownerRevocations != 1 {
		t.Fatalf("failed Workspace was not revoked and stopped: %+v revocations=%d err=%v", failedSetup, ownerRevocations, err)
	}
	if err := pool.QueryRow(ctx, `SELECT t.state,a.state FROM workflow_tasks t
		JOIN workflow_attempts a ON a.organization_id=t.organization_id AND a.task_id=t.id
		WHERE t.organization_id=$1 AND t.run_id=$2 AND a.id=$3`, orgID, fourthRun.ID,
		failedSetup.Outcomes[0].AttemptID).Scan(&taskState, &attemptState); err != nil ||
		taskState != "pending" || attemptState != "stopped" {
		t.Fatalf("failed Workspace became retryable before actor stop: %s/%s, %v", taskState, attemptState, err)
	}
	if _, ok := ax.attemptWorkspaces[axbridge.AttemptWorkspaceName(failedSetup.Outcomes[0].AttemptID)]; ok {
		t.Fatal("failed attempt Workspace was retained after actor-gone proof")
	}
	if _, ok := ax.attemptGateways[axbridge.AttemptGatewayName(failedSetup.Outcomes[0].AttemptID)]; ok || ax.task != nil {
		t.Fatal("failed attempt AX resources were retained after actor-gone proof")
	}

	// Private source: the run froze a Git connection. Without a git.read grant
	// dispatch refuses at preflight with a clear reason and reserves nothing.
	const repo = "https://github.com/owner/repo"
	privateRun, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID,
		LaunchKey: "private-source", SourceCommit: bundle.Source.Commit,
		BundleSHA256: bundle.Digest, VerificationSHA256: policySHA})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles
		(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref,git_connection_id)
		VALUES ($1,$2,$3,$4,$5,'main','git-connection')`, orgID, privateRun.ID, bundleJSON, policyJSON, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTask(ctx, orgID, privateRun.ID, "plan", inputSHA, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, orgID, privateRun.ID); err != nil {
		t.Fatal(err)
	}
	// Leave only the private run dispatchable.
	if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET state='cancelled' WHERE organization_id=$1 AND run_id<>$2 AND state='pending'`,
		orgID, privateRun.ID); err != nil {
		t.Fatal(err)
	}
	ax.task, ax.delayWorkspaceReady = nil, false
	dispatcher.Activate = func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error {
		return nil
	}
	dispatcher.ReleaseModel = func(context.Context, workflow.Attempt, bootstrap.Runtime, workflow.RuntimeBinding, access.ModelInvoke) error {
		return nil
	}
	refused, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(refused.Outcomes) != 1 || refused.Outcomes[0].State != "blocked" ||
		!errors.Is(refused.Outcomes[0].Err, workflow.ErrGitConnection) || refused.Outcomes[0].AttemptID != "" {
		t.Fatalf("private source without a grant was dispatched: %+v, %v", refused, err)
	}
	// An owner selects the connection: the dispatcher gets git.read/git.write.
	if _, err := pool.Exec(ctx, `INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,'git-provider','git','https://github.com',ARRAY['native_raw'],'active')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,'git-connection','organization',$1,'git-provider','x-access-token','token','active')`, orgID); err != nil {
		t.Fatal(err)
	}
	if _, err := secretStore.Rotate(ctx, orgID, "git-connection", 0, []byte("private-git-token"), nil); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.SetProjectGit(ctx, tx, orgID, projectID, "git-connection", repo, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	private, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(private.Outcomes) != 1 || private.Outcomes[0].State != "started" {
		t.Fatalf("private source dispatch: %+v, %v", private, err)
	}
	read, username, isPrivate, err := access.AttemptGitRead(ctx, pool, orgID, private.Outcomes[0].AttemptID)
	if err != nil || !isPrivate || read.RepoURL != repo || read.Commit != bundle.Source.Commit || username != "x-access-token" ||
		read.GranteeID != access.DispatcherGrantee {
		t.Fatalf("private source did not bind the setup-phase Git lease: %+v %q %v %v", read, username, isPrivate, err)
	}
	// Switching the source back to public revokes the dispatcher's Git grants.
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.SetProjectGit(ctx, tx, orgID, projectID, "", repo, "owner"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := access.PreflightGitRead(ctx, pool, orgID, projectID, "git-connection", repo); !errors.Is(err, access.ErrDenied) {
		t.Fatalf("public source kept a Git grant: %v", err)
	}

	// Codex subscription (oauth_access): Alice's personal ChatGPT sign-in is
	// bound only for runs Alice launched; Bob's run in the same project gets
	// the project's workload key.
	codexSubscriptionBindsOnlyForOwnersRun(t, ctx, pool, store, secretStore, ax, &dispatcher, orgID, projectID,
		bundleJSON, policyJSON, bundle.Source.Commit, bundle.Digest, policySHA, inputSHA)
}

func codexSubscriptionBindsOnlyForOwnersRun(t *testing.T, ctx context.Context, pool *pgxpool.Pool, store *workflow.Store,
	secretStore *access.SecretStore, ax *dispatchAX, dispatcher *Dispatcher, orgID, projectID string,
	bundleJSON, policyJSON []byte, sourceCommit, bundleDigest, policySHA, inputSHA string) {
	t.Helper()
	var alice, bob string
	if err := pool.QueryRow(ctx, `SELECT gen_random_uuid()::text, gen_random_uuid()::text`).Scan(&alice, &bob); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`UPDATE access_provider_registrations SET delivery_modes=ARRAY['native_raw','oauth_access'] WHERE organization_id=$1 AND id='openai'`,
		`UPDATE access_project_policies SET delivery_modes=ARRAY['native_raw','oauth_access'] WHERE organization_id=$1`,
	} {
		if _, err := pool.Exec(ctx, statement, orgID); err != nil {
			t.Fatal(err)
		}
	}
	jwt := func(claims map[string]any) string {
		body, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
	}
	auth, _ := json.Marshal(map[string]any{"OPENAI_API_KEY": nil, "last_refresh": "2026-09-24T00:00:00Z",
		"tokens": map[string]string{"access_token": jwt(map[string]any{"exp": time.Now().Add(2 * time.Hour).Unix()}),
			"refresh_token": "rt-alice", "id_token": jwt(map[string]any{"https://api.openai.com/auth": map[string]string{
				"chatgpt_account_id": "acct-alice", "chatgpt_plan_type": "pro"}})}})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	connection, _, err := access.CreateCodexConnection(ctx, tx, secretStore, orgID, alice, "openai", auth)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO access_grants
		(organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ($1,'grant-alice-codex',$2,$3,'user',$4,'model.invoke','openai/gpt-6-luna','oauth_access',$4)`,
		orgID, connection, projectID, alice); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var invokes []access.ModelInvoke
	dispatcher.Activate = func(_ context.Context, _ workflow.Attempt, _ bootstrap.Runtime, _ workflow.RuntimeBinding, invoke access.ModelInvoke) error {
		invokes = append(invokes, invoke)
		return nil
	}
	dispatchFor := func(launchKey, initiator string) (string, access.ModelInvoke) {
		t.Helper()
		run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: orgID, ProjectID: projectID, LaunchKey: launchKey,
			SourceCommit: sourceCommit, BundleSHA256: bundleDigest, VerificationSHA256: policySHA})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles
			(organization_id,run_id,bundle_json,verification_json,repository_url,git_ref)
			VALUES ($1,$2,$3,$4,'https://github.com/owner/repo','main')`, orgID, run.ID, bundleJSON, policyJSON); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AddTask(ctx, orgID, run.ID, "plan", inputSHA, 2); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true,initiator_principal_id=NULLIF($3,'') WHERE organization_id=$1 AND id=$2`,
			orgID, run.ID, initiator); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET state='cancelled' WHERE organization_id=$1 AND run_id<>$2 AND state='pending'`,
			orgID, run.ID); err != nil {
			t.Fatal(err)
		}
		ax.task = nil
		invokes = nil
		batch, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
		if err != nil || len(batch.Outcomes) != 1 || batch.Outcomes[0].State != "started" || len(invokes) != 1 {
			t.Fatalf("%s dispatch: %+v %v", launchKey, batch, err)
		}
		var grant string
		if err := pool.QueryRow(ctx, `SELECT grant_id FROM access_bindings WHERE organization_id=$1 AND attempt_id=$2`,
			orgID, batch.Outcomes[0].AttemptID).Scan(&grant); err != nil {
			t.Fatal(err)
		}
		return grant, invokes[0]
	}
	grant, invoke := dispatchFor("codex-alice", alice)
	if grant != "grant-alice-codex" || invoke.GranteeKind != "user" || invoke.GranteeID != alice {
		t.Fatalf("alice's run did not bind her Codex subscription: grant=%s invoke=%+v", grant, invoke)
	}
	check, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	decision, err := access.AuthorizeModelInvoke(ctx, check, invoke)
	_ = check.Rollback(ctx)
	if err != nil || decision.DeliveryMode != "oauth_access" || decision.ConnectionID != connection {
		t.Fatalf("alice's binding is not an oauth_access delivery: %+v %v", decision, err)
	}
	grant, invoke = dispatchFor("codex-bob", bob)
	if grant != "grant" || invoke.GranteeKind != "workload" || invoke.GranteeID == bob {
		t.Fatalf("bob's run bound alice's Codex subscription: grant=%s invoke=%+v", grant, invoke)
	}
	if grant, _ = dispatchFor("codex-no-initiator", ""); grant != "grant" {
		t.Fatalf("a run without an initiator bound a personal subscription: %s", grant)
	}
}

func dispatchSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
