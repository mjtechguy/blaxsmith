package dispatch

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type dispatchAX struct {
	task                       *axbridge.Task
	workspace                  axbridge.Workspace
	attemptWorkspaces          map[string]axbridge.Workspace
	gateway                    axbridge.Gateway
	attemptGateways            map[string]axbridge.Gateway
	invalidateGatewayAfterTask bool
}

func (a *dispatchAX) Get(context.Context, string, string) (axbridge.Task, error) {
	if a.task == nil {
		return axbridge.Task{}, axbridge.ErrNotFound
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
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanup, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE")
	})
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
		}}
	withoutActivation := dispatcher
	withoutActivation.Activate = nil
	if _, err := withoutActivation.DispatchBatch(ctx, "", 1, 1); err != ErrNotReady {
		t.Fatalf("missing bootstrap activator must block dispatch: %v", err)
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
	batch, err := dispatcher.DispatchBatch(ctx, "", 1, 1)
	if err != nil || len(batch.Outcomes) != 1 || batch.Outcomes[0].State != "started" ||
		batch.Outcomes[0].AttemptID == "" || batch.Outcomes[0].BindingID == "" || ax.task == nil || activationPreflights != 1 {
		t.Fatalf("dispatch batch: %+v, %v", batch, err)
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
}

func dispatchSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
