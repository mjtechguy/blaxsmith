package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestAdminDashboardIsScopedDeniesNonAdminsAndHidesSecrets(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "admin-a")
	other := organization(t, pool, "admin-b")
	owner := reviewer(t, pool, org, "owner", "admin-owner")
	admin := reviewer(t, pool, org, "admin", "admin-admin")
	member := reviewer(t, pool, org, "member", "admin-member")
	viewer := reviewer(t, pool, org, "viewer", "admin-viewer")
	outsider := reviewer(t, pool, other, "owner", "admin-outsider")

	project, err := store.CreateProject(ctx, org, "admin-project", "Admin project")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_audit_events (organization_id,actor_kind,actor_id,action,subject_id)
		VALUES ($1,'principal',$2,'workflow.project.created',$3)`, org, owner.PrincipalID, project); err != nil {
		t.Fatal(err)
	}
	input := RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "admin-run",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)}
	run, err := store.CreateRun(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "build", input.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles (organization_id,run_id,bundle_json,verification_json)
		VALUES ($1,$2,$3::jsonb,'{}')`, org, run.ID,
		`{"recipe":{"profiles":{"p":{"harness":"codex","model":"gpt-5"}},"stages":[{"id":"build","kind":"implement","profile":"p"}]}}`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_attempts SET control_holder_principal_id=$3,control_holder_session_id=$4,
		control_generation=1 WHERE organization_id=$1 AND id=$2`, org, attempt.ID, admin.PrincipalID, admin.SessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_interactions (organization_id,run_id,task_id,attempt_id,origin,origin_key,kind,payload)
		VALUES ($1,$2,$3,$4,'guest','q1','question','{"id":"q1","kind":"question","title":"Which scope?","blocking":true,"allow_free_text":true}')`,
		org, run.ID, task, attempt.ID); err != nil {
		t.Fatal(err)
	}
	secrets, err := access.NewSecretStore(pool, "primary", map[string][]byte{"primary": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	const modelSecret, gitSecret = "sk-admin-model-secret-value", "ghp-admin-git-secret-token"
	model, err := store.CreateProjectModelAccessAs(ctx, owner, project, "openai", "gpt-5", []byte(modelSecret), secrets)
	if err != nil {
		t.Fatal(err)
	}
	git, err := store.CreateGitConnectionAs(ctx, owner, "github.com", "octo-bot", []byte(gitSecret), secrets)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := access.SetProjectGit(ctx, tx, org, project, git.ID, "https://github.com/example/admin.git", owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	for _, caller := range []identity.Caller{member, viewer} {
		if _, err := store.AdminOverview(ctx, caller); !errors.Is(err, ErrAdminDenied) {
			t.Fatalf("%s read overview: %v", caller.Role, err)
		}
		if _, err := store.ListAuditEvents(ctx, caller, AuditFilter{Limit: 10}); !errors.Is(err, ErrAdminDenied) {
			t.Fatalf("%s read audit: %v", caller.Role, err)
		}
		if _, err := store.HaltRunAs(ctx, caller, run.ID); !errors.Is(err, ErrAdminDenied) {
			t.Fatalf("%s halted run: %v", caller.Role, err)
		}
		if err := store.RevokeGrantAs(ctx, caller, model.GrantID); !errors.Is(err, ErrAdminDenied) {
			t.Fatalf("%s revoked grant: %v", caller.Role, err)
		}
	}
	// A token whose role was downgraded since issue is fenced under lock.
	stale := admin
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2`,
		org, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HaltRunAs(ctx, stale, run.ID); !errors.Is(err, ErrFenced) {
		t.Fatalf("downgraded admin halted run: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='admin' WHERE organization_id=$1 AND principal_id=$2`,
		org, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}

	overview, err := store.AdminOverview(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.LiveAttempts) != 1 {
		t.Fatalf("live attempts: %+v", overview.LiveAttempts)
	}
	live := overview.LiveAttempts[0]
	if live.AttemptID != attempt.ID || live.ProjectName != "Admin project" || live.Stage != "build" || live.Kind != "implement" ||
		live.Harness != "codex" || live.Model != "gpt-5" || live.State != "running" || live.ControllerUsername != "admin-admin" ||
		live.LastActivity == nil {
		t.Fatalf("live attempt row: %+v", live)
	}
	if len(overview.OpenInteractions) != 1 || overview.OpenInteractions[0].Title != "Which scope?" ||
		!overview.OpenInteractions[0].Blocking || overview.OpenInteractions[0].Stage != "build" {
		t.Fatalf("open interactions: %+v", overview.OpenInteractions)
	}
	if overview.RunStates["waiting_on_human"] != 1 || overview.RunStates["running"] != 0 {
		t.Fatalf("run states: %+v", overview.RunStates)
	}
	if overview.Capacity != (AdminCapacity{InFlight: 1, Running: 1, TakenOver: 1}) {
		t.Fatalf("capacity: %+v", overview.Capacity)
	}
	if len(overview.Connections) != 2 || len(overview.Grants) != 3 {
		t.Fatalf("connections %+v grants %+v", overview.Connections, overview.Grants)
	}
	for _, c := range overview.Connections {
		if c.ActiveGrants < 1 || c.State != "active" || (c.ProviderKind == "git" && (c.Host != "github.com" || c.Account != "octo-bot")) {
			t.Fatalf("connection health: %+v", c)
		}
	}
	data, err := json.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	var ciphertexts []string
	rows, err := pool.Query(ctx, `SELECT encode(ciphertext,'base64') FROM access_secret_versions WHERE organization_id=$1`, org)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		ciphertexts = append(ciphertexts, c)
	}
	rows.Close()
	for _, secret := range append([]string{modelSecret, gitSecret}, ciphertexts...) {
		if strings.Contains(string(data), secret) {
			t.Fatalf("overview leaked secret material %q", secret)
		}
	}

	otherView, err := store.AdminOverview(ctx, outsider)
	if err != nil || len(otherView.LiveAttempts)+len(otherView.OpenInteractions)+len(otherView.Connections)+len(otherView.Grants) != 0 ||
		len(otherView.RunStates) != 0 || otherView.Capacity != (AdminCapacity{}) {
		t.Fatalf("other organization saw rows: %+v, %v", otherView, err)
	}
	if events, err := store.ListAuditEvents(ctx, outsider, AuditFilter{Limit: 50}); err != nil || len(events) != 0 {
		t.Fatalf("other organization audit: %+v, %v", events, err)
	}
	if _, err := store.HaltRunAs(ctx, outsider, run.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other organization halted run: %v", err)
	}
	if err := store.RevokeGrantAs(ctx, outsider, model.GrantID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other organization revoked grant: %v", err)
	}

	// Revoke the Git read grant, then the model grant through its existing path.
	var gitRead string
	for _, g := range overview.Grants {
		if g.Capability == "git.read" {
			gitRead = g.ID
		}
		if g.ProjectName != "Admin project" {
			t.Fatalf("grant project: %+v", g)
		}
	}
	if err := store.RevokeGrantAs(ctx, admin, gitRead); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeGrantAs(ctx, admin, gitRead); !errors.Is(err, ErrNotFound) {
		t.Fatalf("repeated git revocation: %v", err)
	}
	if err := store.RevokeGrantAs(ctx, owner, model.GrantID); err != nil {
		t.Fatal(err)
	}
	if items, err := store.ListProjectModelAccess(ctx, org, project); err != nil || len(items) != 0 {
		t.Fatalf("model selection survived revocation: %+v, %v", items, err)
	}
	if after, err := store.AdminOverview(ctx, owner); err != nil || len(after.Grants) != 1 || after.Grants[0].Capability != "git.write" {
		t.Fatalf("grants after revocation: %+v, %v", after.Grants, err)
	}

	// Halt: pending work cancels now, the live owner is stopped by the sweep,
	// and Progress closes the run once no owner remains.
	if state, err := store.HaltRunAs(ctx, owner, run.ID); err != nil || state != "cancel_requested" {
		t.Fatalf("halt: %q, %v", state, err)
	}
	if _, err := store.HaltRunAs(ctx, admin, run.ID); err != nil {
		t.Fatalf("repeated halt: %v", err)
	}
	if _, err := store.Progress(ctx, "", "", 100, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetRun(ctx, org, run.ID); got.State != "cancel_requested" {
		t.Fatalf("run closed with a live owner: %s", got.State)
	}
	if err := store.ConfirmStopped(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Progress(ctx, "", "", 100, nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetRun(ctx, org, run.ID); got.State != "cancelled" {
		t.Fatalf("halted run state: %s", got.State)
	}
	if after, err := store.AdminOverview(ctx, owner); err != nil || len(after.LiveAttempts) != 0 ||
		len(after.OpenInteractions) != 0 || after.RunStates["halted"] != 1 {
		t.Fatalf("overview after halt: %+v, %v", after, err)
	}

	// Audit: project resolution, filters, and keyset paging.
	all, err := store.ListAuditEvents(ctx, owner, AuditFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, e := range all {
		count[e.Action]++
		switch e.Action {
		case "workflow.run.halted", "access.grant.revoked", "access.project_model.revoked", "access.project_model.created", "workflow.project.created":
			if e.ProjectID != project || e.Name != "Admin project" {
				t.Fatalf("audit project not resolved: %+v", e)
			}
		case "access.git_connection.created":
			if e.ProjectID != "" {
				t.Fatalf("org-level event got a project: %+v", e)
			}
		}
	}
	if count["workflow.run.halted"] != 1 || count["access.grant.revoked"] != 1 || count["access.project_model.revoked"] != 1 {
		t.Fatalf("audit actions: %v", count)
	}
	byProject, err := store.ListAuditEvents(ctx, owner, AuditFilter{ProjectID: project, Limit: 100})
	if err != nil || len(byProject) != len(all)-1 {
		t.Fatalf("project filter: %d of %d, %v", len(byProject), len(all), err)
	}
	byActor, err := store.ListAuditEvents(ctx, owner, AuditFilter{Actor: "ADMIN-admin", Limit: 100})
	if err != nil || len(byActor) != 1 || byActor[0].Action != "access.grant.revoked" || byActor[0].ActorUsername != "admin-admin" {
		t.Fatalf("actor filter: %+v, %v", byActor, err)
	}
	byAction, err := store.ListAuditEvents(ctx, owner, AuditFilter{Action: "workflow.run.halted", Limit: 100})
	if err != nil || len(byAction) != 1 {
		t.Fatalf("action filter: %+v, %v", byAction, err)
	}
	first, err := store.ListAuditEvents(ctx, owner, AuditFilter{Limit: 2})
	if err != nil || len(first) != 2 || first[0].ID <= first[1].ID {
		t.Fatalf("first page: %+v, %v", first, err)
	}
	next, err := store.ListAuditEvents(ctx, owner, AuditFilter{Limit: 100, BeforeID: first[1].ID})
	if err != nil || len(next) != len(all)-2 || next[0].ID >= first[1].ID {
		t.Fatalf("next page: %d, %v", len(next), err)
	}
	if _, err := store.ListAuditEvents(ctx, owner, AuditFilter{Limit: 10, ProjectID: "not-a-uuid"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid project filter: %v", err)
	}
}
