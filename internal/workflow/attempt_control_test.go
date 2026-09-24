package workflow

import (
	"errors"
	"strings"
	"testing"
)

func TestAttemptControlFencePostgres(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	org, other := organization(t, pool, "control-a"), organization(t, pool, "control-b")
	owner := reviewer(t, pool, org, "owner", "control-owner")
	member := reviewer(t, pool, org, "member", "control-member")
	viewer := reviewer(t, pool, org, "viewer", "control-viewer")
	admin := reviewer(t, pool, org, "admin", "control-admin")
	stranger := reviewer(t, pool, org, "member", "control-stranger")
	outsider := reviewer(t, pool, other, "owner", "control-outsider")
	project, err := store.CreateProject(ctx, org, "control-project", "Control project")
	if err != nil {
		t.Fatal(err)
	}
	input := RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "control-run",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64),
		VerificationSHA256: strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "implement", input.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.TakeOverAttempt(ctx, owner, attempt.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("takeover before running: %v", err)
	}
	if err := store.BindRuntime(ctx, attempt, RuntimeBinding{AXAtespace: "blaxsmith-org", AXTask: "attempt-1",
		ActorUID: "uid", TemplateUID: "tpl", Image: "runner@sha256:" + strings.Repeat("d", 64),
		WorkerPool: "pool", CommandSHA256: strings.Repeat("e", 64)}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}

	if _, _, err := store.TakeOverAttempt(ctx, viewer, attempt.ID); !errors.Is(err, ErrAttemptControlDenied) {
		t.Fatalf("viewer took control: %v", err)
	}
	if _, _, err := store.TakeOverAttempt(ctx, outsider, attempt.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant takeover: %v", err)
	}
	held, changed, err := store.TakeOverAttempt(ctx, owner, attempt.ID)
	if err != nil || !changed || held.HolderPrincipalID != owner.PrincipalID || held.HolderSessionID != owner.SessionID ||
		held.ControlGeneration != 1 || !held.Human() || held.Stage != "implement" || held.Actor != "attempt-1" {
		t.Fatalf("takeover: %+v %v %v", held, changed, err)
	}
	// A retry or reconnect by the same session never mints a second controller.
	again, changed, err := store.TakeOverAttempt(ctx, owner, attempt.ID)
	if err != nil || changed || again.ControlGeneration != 1 {
		t.Fatalf("repeat takeover: %+v %v %v", again, changed, err)
	}
	if _, _, err := store.TakeOverAttempt(ctx, admin, attempt.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("second takeover: %v", err)
	}
	if _, err := store.ReleaseAttemptControl(ctx, member, attempt.ID, 1); !errors.Is(err, ErrAttemptControlDenied) {
		t.Fatalf("non-holder handback: %v", err)
	}
	if _, err := store.ReleaseAttemptControl(ctx, owner, attempt.ID, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale-generation handback: %v", err)
	}
	released, err := store.ReleaseAttemptControl(ctx, owner, attempt.ID, 1)
	if err != nil || released.Human() || released.ControlGeneration != 2 {
		t.Fatalf("handback: %+v %v", released, err)
	}
	// Takeover discloses the model key: a member needs to own the attempt's
	// personal model connection; owners and admins always may.
	if ok, err := store.CanTakeOverAttempt(ctx, member, attempt.ID); err != nil || ok {
		t.Fatalf("member without a personal connection may take over: %v %v", ok, err)
	}
	if _, _, err := store.TakeOverAttempt(ctx, member, attempt.ID); !errors.Is(err, ErrAttemptControlDenied) {
		t.Fatalf("member without a personal connection took control: %v", err)
	}
	// The member's personal model connection, bound to this attempt.
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state)
		VALUES ($1,'reg','anthropic','https://api.anthropic.com',ARRAY['native_raw'],'active')`, []any{org}},
		{`INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state)
		VALUES ($1,'personal','user',$2,'reg','acct','api_key','active')`, []any{org, member.PrincipalID}},
		{`INSERT INTO access_project_policies (organization_id,project_id,version,git_read_enabled,delivery_modes)
		VALUES ($1,$2,1,false,ARRAY['native_raw'])`, []any{org, project}},
		{`INSERT INTO access_grants (organization_id,id,connection_id,project_id,grantee_kind,grantee_id,capability,resource,delivery_mode,issuer_id)
		VALUES ($1,'grant','personal',$2,'workload','blaxsmith-dispatcher','model.invoke','anthropic/claude','native_raw',$3)`,
			[]any{org, project, member.PrincipalID}},
		{`INSERT INTO access_bindings (organization_id,id,attempt_id,project_id,grant_id,grant_version,capability,resource,policy_version)
		VALUES ($1,'binding',$2,$3,'grant',1,'model.invoke','anthropic/claude',1)`, []any{org, attempt.ID, project}},
	} {
		if _, err := pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := store.TakeOverAttempt(ctx, stranger, attempt.ID); !errors.Is(err, ErrAttemptControlDenied) {
		t.Fatalf("member who does not own the connection took control: %v", err)
	}
	if ok, err := store.CanTakeOverAttempt(ctx, member, attempt.ID); err != nil || !ok {
		t.Fatalf("personal owner may not take over: %v %v", ok, err)
	}
	if next, changed, err := store.TakeOverAttempt(ctx, member, attempt.ID); err != nil || !changed || next.ControlGeneration != 3 {
		t.Fatalf("personal owner takeover after handback: %+v %v %v", next, changed, err)
	}
	if _, err := store.ReleaseAttemptControl(ctx, member, attempt.ID, 3); err != nil {
		t.Fatal(err)
	}
	if next, changed, err := store.TakeOverAttempt(ctx, admin, attempt.ID); err != nil || !changed || next.ControlGeneration != 5 {
		t.Fatalf("admin takeover: %+v %v %v", next, changed, err)
	}
	var audits, events int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND subject_id=$2 AND action LIKE 'workflow.attempt.control_%'`, org, attempt.ID).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_events WHERE organization_id=$1
		AND attempt_id=$2 AND kind='attempt.control'`, org, attempt.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if audits != 5 || events != 5 {
		t.Fatalf("audit %d, events %d", audits, events)
	}
}
