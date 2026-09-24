package workflow

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func TestWorkspaceViewsAreScopedRoleAwareAndPaged(t *testing.T) {
	pool := testPool(t)
	ctx := t.Context()
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "workspace-a")
	other := organization(t, pool, "workspace-b")
	owner := reviewer(t, pool, org, "owner", "ws-owner")
	admin := reviewer(t, pool, org, "admin", "ws-admin")
	member := reviewer(t, pool, org, "member", "ws-member")
	viewer := reviewer(t, pool, org, "viewer", "ws-viewer")
	outsider := reviewer(t, pool, other, "owner", "ws-outsider")

	alpha, err := store.CreateProject(ctx, org, "alpha-proj", "Alpha project")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := store.CreateProject(ctx, org, "beta-proj", "Beta project")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := store.CreateProject(ctx, other, "foreign-proj", "Foreign project")
	if err != nil {
		t.Fatal(err)
	}
	newRun := func(orgID, project, key string) (Run, string) {
		t.Helper()
		input := RunInput{OrganizationID: orgID, ProjectID: project, LaunchKey: key,
			SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)}
		run, err := store.CreateRun(ctx, input)
		if err != nil {
			t.Fatal(err)
		}
		task, err := store.AddTask(ctx, orgID, run.ID, "build", input.BundleSHA256, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles (organization_id,run_id,bundle_json,verification_json)
			VALUES ($1,$2,$3::jsonb,'{}')`, orgID, run.ID,
			`{"recipe":{"profiles":{"p":{"harness":"codex","model":"gpt-5"}},"stages":[{"id":"build","kind":"implement","profile":"p"}]}}`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`,
			orgID, run.ID); err != nil {
			t.Fatal(err)
		}
		return run, task
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	interaction := func(orgID, runID, task, key, kind, payload string) {
		t.Helper()
		exec(`INSERT INTO workflow_interactions (organization_id,run_id,task_id,origin,origin_key,kind,payload)
			VALUES ($1,$2,$3,'platform',$4,$5,$6::jsonb)`, orgID, runID, task, key, kind, payload)
	}

	// Oldest first: a queued run with a blocking question on the build stage.
	question, questionTask := newRun(org, alpha, "run-question")
	interaction(org, question.ID, questionTask, "q1", "question", `{"title":"Which scope?","blocking":true}`)
	// An active run whose live attempt asks a non-blocking approval.
	approval, approvalTask := newRun(org, alpha, "run-approval")
	attempt, err := store.ReserveAttempt(ctx, org, approval.ID, approvalTask)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE workflow_attempts SET control_holder_principal_id=$3,control_holder_session_id=$4,control_generation=1
		WHERE organization_id=$1 AND id=$2`, org, attempt.ID, admin.PrincipalID, admin.SessionID)
	interaction(org, approval.ID, approvalTask, "a1", "approval", `{"title":"Ship it?","blocking":false}`)
	// A succeeded run with an undecided review, one with a decided review, a
	// failed run, a done run, and an escalated stage.
	review, _ := newRun(org, beta, "run-review")
	decided, _ := newRun(org, beta, "run-decided")
	failed, _ := newRun(org, beta, "run-failed")
	done, _ := newRun(org, beta, "run-done")
	_, escalatedTask := newRun(org, beta, "run-escalated")
	exec(`UPDATE workflow_runs SET state='succeeded' WHERE organization_id=$1 AND id IN ($2,$3,$4)`,
		org, review.ID, decided.ID, done.ID)
	exec(`UPDATE workflow_runs SET state='failed' WHERE organization_id=$1 AND id=$2`, org, failed.ID)
	exec(`UPDATE workflow_tasks SET state='escalated' WHERE organization_id=$1 AND id=$2`, org, escalatedTask)
	pkg, err := store.PresentForReview(ctx, org, review.ID, strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	decidedPkg, err := store.PresentForReview(ctx, org, decided.ID, strings.Repeat("d", 40), strings.Repeat("e", 64), strings.Repeat("c", 64))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DecideReview(ctx, owner, decided.ID, decidedPkg.ID, "ws-decide", "approve", ""); err != nil {
		t.Fatal(err)
	}
	// Another organization's open work never appears.
	foreignRun, foreignTask := newRun(other, foreign, "run-foreign")
	interaction(other, foreignRun.ID, foreignTask, "f1", "question", `{"title":"Foreign?","blocking":true}`)

	// Inbox: blocking first, then oldest; can_act follows role.
	for _, tc := range []struct {
		caller         identity.Caller
		answer, review bool
		actionable     int32
	}{{owner, true, true, 3}, {admin, true, true, 3}, {member, true, false, 2}, {viewer, false, false, 0}} {
		items, total, err := store.ListInbox(ctx, tc.caller, InboxFilter{})
		if err != nil || total != 3 || len(items) != 3 {
			t.Fatalf("%s inbox: %+v %d %v", tc.caller.Role, items, total, err)
		}
		// The question and review block; the review was presented later.
		if items[0].ID == "" || items[0].Kind != "question" || items[0].Title != "Which scope?" || !items[0].Blocking ||
			items[0].Stage != "build" || items[0].ProjectName != "Alpha project" || items[0].LaunchKey != "run-question" ||
			items[1].Kind != "review" || items[1].ID != pkg.ID || items[1].Title != "Review package revision 1" ||
			items[1].Stage != "" || items[1].RunID != review.ID || items[2].Kind != "approval" || items[2].Blocking {
			t.Fatalf("%s inbox order: %+v", tc.caller.Role, items)
		}
		for _, i := range items {
			if i.Kind == "review" && i.CanAct != tc.review {
				t.Fatalf("%s can act on review: %+v", tc.caller.Role, i)
			} else if i.Kind != "review" && i.CanAct != tc.answer {
				t.Fatalf("%s can act on %s: %+v", tc.caller.Role, i.Kind, i)
			}
		}
		actionable, total, err := store.ListInbox(ctx, tc.caller, InboxFilter{ActionableOnly: true})
		if err != nil || total != tc.actionable || int32(len(actionable)) != tc.actionable {
			t.Fatalf("%s actionable: %+v %d %v", tc.caller.Role, actionable, total, err)
		}
		for _, i := range actionable {
			if !i.CanAct {
				t.Fatalf("%s actionable item cannot act: %+v", tc.caller.Role, i)
			}
		}
	}
	for _, tc := range []struct {
		f    InboxFilter
		want []string
	}{
		{InboxFilter{Kinds: []string{"review"}}, []string{pkg.ID}},
		{InboxFilter{Kinds: []string{"question", "approval"}, ProjectID: alpha}, []string{"question", "approval"}},
		{InboxFilter{ProjectID: beta}, []string{pkg.ID}},
		{InboxFilter{Search: "SHIP"}, []string{"approval"}},
		{InboxFilter{Search: "beta proj"}, []string{pkg.ID}},
		{InboxFilter{Search: "run-question"}, []string{"question"}},
		{InboxFilter{Search: "build"}, []string{"question", "approval"}},
		{InboxFilter{Search: "nothing-matches"}, nil},
		{InboxFilter{PageSize: 1, Page: 2}, []string{pkg.ID}},
		{InboxFilter{PageSize: 2, Page: 3}, nil},
	} {
		items, total, err := store.ListInbox(ctx, owner, tc.f)
		if err != nil {
			t.Fatalf("inbox %+v: %v", tc.f, err)
		}
		var got []string
		for _, i := range items {
			if i.Kind == "review" {
				got = append(got, i.ID)
			} else {
				got = append(got, i.Kind)
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Fatalf("inbox %+v: %v, want %v", tc.f, got, tc.want)
		}
		if tc.f.Page < 2 && total != int32(len(tc.want)) || tc.f.Page >= 2 && total != 3 {
			t.Fatalf("inbox %+v total %d", tc.f, total)
		}
	}
	for _, f := range []InboxFilter{{Page: -1}, {PageSize: -1}, {PageSize: 101}, {Page: 10002, PageSize: 1},
		{Kinds: []string{"poll"}}, {ProjectID: "not-a-uuid"}, {Search: strings.Repeat("x", 121)}} {
		if _, _, err := store.ListInbox(ctx, owner, f); !errors.Is(err, ErrInvalid) {
			t.Fatalf("inbox %+v accepted: %v", f, err)
		}
	}
	if items, total, err := store.ListInbox(ctx, outsider, InboxFilter{}); err != nil || total != 1 || items[0].Title != "Foreign?" {
		t.Fatalf("outsider inbox: %+v %d %v", items, total, err)
	}
	if items, total, err := store.ListInbox(ctx, outsider, InboxFilter{ProjectID: alpha}); err != nil || total != 0 || len(items) != 0 {
		t.Fatalf("outsider filtered by foreign project: %+v %d %v", items, total, err)
	}
	stale := owner
	stale.SessionID = ""
	if _, _, err := store.ListInbox(ctx, stale, InboxFilter{}); !errors.Is(err, ErrFenced) {
		t.Fatalf("malformed caller: %v", err)
	}

	// Runs: every role reads every project; status rolls up on the server.
	want := map[string]struct {
		status        string
		open          int32
		reviewWaiting bool
	}{
		"run-question": {"awaiting_input", 1, false}, "run-approval": {"needs_approval", 1, false},
		"run-review": {"needs_approval", 0, true}, "run-decided": {"done", 0, false}, "run-failed": {"failed", 0, false},
		"run-done": {"done", 0, false}, "run-escalated": {"awaiting_input", 0, false},
	}
	for _, caller := range []identity.Caller{owner, admin, member, viewer} {
		runs, total, err := store.ListWorkspaceRuns(ctx, caller, RunFilter{})
		if err != nil || total != 7 || len(runs) != 7 {
			t.Fatalf("%s runs: %+v %d %v", caller.Role, runs, total, err)
		}
		if runs[0].LaunchKey != "run-escalated" || runs[6].LaunchKey != "run-question" {
			t.Fatalf("%s default order: %+v", caller.Role, runs)
		}
		for _, r := range runs {
			w := want[r.LaunchKey]
			if r.Status != w.status || r.OpenInteractions != w.open || r.ReviewWaiting != w.reviewWaiting || r.StageCount != 1 ||
				r.ProjectName == "" || r.SourceCommit != strings.Repeat("a", 40) {
				t.Fatalf("%s run %s: %+v", caller.Role, r.LaunchKey, r)
			}
		}
	}
	for _, tc := range []struct {
		f     RunFilter
		want  string
		total int32
	}{
		{RunFilter{SortBy: "launch_key", SortDirection: "asc"},
			"[run-approval run-decided run-done run-escalated run-failed run-question run-review]", 7},
		{RunFilter{SortBy: "launch_key", SortDirection: "desc", PageSize: 2, Page: 2}, "[run-failed run-escalated]", 7},
		{RunFilter{SortBy: "project", SortDirection: "desc", States: []string{"active", "failed"}}, "[run-failed run-approval]", 2},
		{RunFilter{SortBy: "state", SortDirection: "asc", States: []string{"failed", "active"}}, "[run-approval run-failed]", 2},
		{RunFilter{SortBy: "created_at", SortDirection: "asc", States: []string{"queued"}}, "[run-question run-escalated]", 2},
		{RunFilter{States: []string{"queued"}, ProjectID: beta}, "[run-escalated]", 1},
		{RunFilter{Search: "ALPHA"}, "[run-approval run-question]", 2},
		{RunFilter{Search: "decided"}, "[run-decided]", 1},
		{RunFilter{Search: strings.Repeat("a", 12)},
			"[run-escalated run-done run-failed run-decided run-review run-approval run-question]", 7},
		{RunFilter{Page: 5, PageSize: 2}, "[]", 7},
	} {
		runs, total, err := store.ListWorkspaceRuns(ctx, owner, tc.f)
		if err != nil {
			t.Fatalf("runs %+v: %v", tc.f, err)
		}
		keys := []string{}
		for _, r := range runs {
			keys = append(keys, r.LaunchKey)
		}
		if fmt.Sprint(keys) != tc.want || total != tc.total {
			t.Fatalf("runs %+v: %v (%d), want %s (%d)", tc.f, keys, total, tc.want, tc.total)
		}
	}
	for _, f := range []RunFilter{{Page: -1}, {PageSize: 101}, {Page: 102, PageSize: 100}, {SortBy: "name"},
		{SortDirection: "up"}, {States: []string{"running"}}, {ProjectID: "x"}, {Search: strings.Repeat("x", 121)}} {
		if _, _, err := store.ListWorkspaceRuns(ctx, owner, f); !errors.Is(err, ErrInvalid) {
			t.Fatalf("runs %+v accepted: %v", f, err)
		}
	}
	if runs, total, err := store.ListWorkspaceRuns(ctx, outsider, RunFilter{}); err != nil || total != 1 || runs[0].LaunchKey != "run-foreign" {
		t.Fatalf("outsider runs: %+v %d %v", runs, total, err)
	}

	// Home: counts and short lists for the caller's role.
	home, err := store.WorkspaceHome(ctx, member)
	if err != nil {
		t.Fatal(err)
	}
	if home.WaitingOnYou != 2 || home.OpenItems != 3 || home.RunningAgents != 1 || home.ActiveRuns != 3 ||
		home.RunsLast24h != 7 || home.FailedLast24h != 1 || len(home.Waiting) != 2 || home.Waiting[0].Kind != "question" ||
		len(home.Agents) != 1 || len(home.RecentRuns) != 7 || home.RecentRuns[0].LaunchKey != "run-escalated" {
		t.Fatalf("member home: %+v", home)
	}
	if a := home.Agents[0]; a.AttemptID != attempt.ID || a.Kind != "implement" || a.Harness != "codex" || a.Model != "gpt-5" ||
		a.ControllerID != admin.PrincipalID || a.ProjectName != "Alpha project" {
		t.Fatalf("home agent: %+v", a)
	}
	if home, err := store.WorkspaceHome(ctx, viewer); err != nil || home.WaitingOnYou != 0 || len(home.Waiting) != 0 || home.OpenItems != 3 {
		t.Fatalf("viewer home: %+v %v", home, err)
	}
	if home, err := store.WorkspaceHome(ctx, owner); err != nil || home.WaitingOnYou != 3 || home.Waiting[1].Kind != "review" {
		t.Fatalf("owner home: %+v %v", home, err)
	}
	if home, err := store.WorkspaceHome(ctx, outsider); err != nil || home.OpenItems != 1 || home.RunningAgents != 0 ||
		len(home.Agents) != 0 || len(home.RecentRuns) != 1 || home.ActiveRuns != 1 {
		t.Fatalf("outsider home: %+v %v", home, err)
	}
}
