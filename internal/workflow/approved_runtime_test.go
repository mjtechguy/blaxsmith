package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestApprovedToolRuntimePostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org, other := organization(t, pool, "runtime"), organization(t, pool, "runtime-other")
	project, err := store.CreateProject(tenant.System(t.Context()), org, "runtime-project", "Runtime project")
	if err != nil {
		t.Fatal(err)
	}
	var approvalID string
	err = pool.QueryRow(tenant.System(t.Context()), `INSERT INTO workflow_tool_runtime_approvals
		(organization_id,harness,model,effort,image,binary_path,binary_sha256,version,worker_pool,
		 max_timeout_seconds,max_output_bytes,approved_by)
		VALUES ($1,'codex','gpt-6-luna','xhigh',$2,'/opt/blaxsmith/bin/codex',$3,'0.156.1','pool-a',1800,1048576,'operator')
		RETURNING id`, org, "runner@sha256:"+strings.Repeat("a", 64), strings.Repeat("b", 64)).Scan(&approvalID)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := store.GetApprovedToolRuntime(tenant.System(t.Context()), org, "codex", "gpt-6-luna", "xhigh")
	if err != nil || approved.ApprovalID != approvalID || approved.Runtime.Image == "" ||
		len(approved.Runtime.Supported) != 1 || approved.WorkerPool != "pool-a" {
		t.Fatalf("approved runtime: %+v, %v", approved, err)
	}
	if _, err := store.GetApprovedToolRuntime(tenant.System(t.Context()), other, "codex", "gpt-6-luna", "xhigh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant approval: %v", err)
	}
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE workflow_tool_runtime_approvals SET revoked_at=clock_timestamp() WHERE id=$1`, approvalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetApprovedToolRuntime(tenant.System(t.Context()), org, "codex", "gpt-6-luna", "xhigh"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked approval remained active: %v", err)
	}
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE workflow_tool_runtime_approvals SET worker_pool='pool-b' WHERE id=$1`, approvalID); err == nil {
		t.Fatal("revoked runtime approval was editable")
	}
	var selectionID string
	if err := pool.QueryRow(tenant.System(t.Context()), `INSERT INTO workflow_project_model_grants
		(organization_id,project_id,provider,model,grant_id,grantee_id,approved_by)
		VALUES ($1,$2,'openai','gpt-6-luna','grant-one','blaxsmith-dispatcher','operator') RETURNING id`,
		org, project).Scan(&selectionID); err != nil {
		t.Fatal(err)
	}
	selection, err := store.GetProjectModelGrant(tenant.System(t.Context()), org, project, "openai", "gpt-6-luna")
	if err != nil || selection.SelectionID != selectionID || selection.GrantID != "grant-one" ||
		selection.GranteeID != "blaxsmith-dispatcher" {
		t.Fatalf("project model grant: %+v, %v", selection, err)
	}
	if _, err := store.GetProjectModelGrant(tenant.System(t.Context()), other, project, "openai", "gpt-6-luna"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant grant selection: %v", err)
	}
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE workflow_project_model_grants SET grant_id='grant-two' WHERE id=$1`, selectionID); err == nil {
		t.Fatal("active grant selection was editable")
	}
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE workflow_project_model_grants SET revoked_at=clock_timestamp() WHERE id=$1`, selectionID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetProjectModelGrant(tenant.System(t.Context()), org, project, "openai", "gpt-6-luna"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked grant selection remained active: %v", err)
	}
}
