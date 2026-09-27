package interact

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestGoalWorkspacePostgres(t *testing.T) {
	f := newFixture(t)
	ctx := tenant.System(t.Context())
	project, err := f.workflow.CreateProject(ctx, f.org, "goal-project", "Goal project")
	if err != nil {
		t.Fatal(err)
	}
	in := GoalInput{ProjectID: project, RequestKey: "create", Title: "A native goal", Brief: "Preserve the existing behavior.", FactoryID: "example-factory", FactoryVersion: "1", Questions: []Interaction{
		{ID: "choice", Kind: "question", Title: "What matters?", AllowFreeText: true, Options: []Option{{ID: "small", Label: "Small"}, {ID: "full", Label: "Full"}}},
		{ID: "required", Kind: "question", Title: "Required input", Blocking: true, AllowFreeText: true},
	}}
	id, err := f.store.CreateGoal(ctx, f.owner, in)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.store.CreateGoal(ctx, f.owner, in)
	if err != nil || again != id {
		t.Fatalf("create retry: %s %v", again, err)
	}
	other := in
	other.Brief = "Different"
	if _, err = f.store.CreateGoal(ctx, f.owner, other); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("reused create key: %v", err)
	}
	g, entries, more, err := f.store.GetGoal(ctx, f.org, id, 0)
	if err != nil || g.Revision != 1 || len(entries) != 0 || more || len(g.Questions) != 2 {
		t.Fatalf("get: %+v %v", g, err)
	}
	var runs int
	if err = f.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE organization_id=$1 AND project_id=$2`, f.org, project).Scan(&runs); err != nil || runs != 0 {
		t.Fatalf("goal started a run: %d %v", runs, err)
	}
	reply := GoalReply{GoalID: id, RequestKey: "answer", ExpectedRevision: 1, Kind: "answer", QuestionID: "choice", OptionIDs: []string{"small"}, Text: "because scope matters"}
	bad := reply
	bad.OptionIDs = []string{"made-up"}
	if err = f.store.ReplyGoal(ctx, f.owner, bad); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatalf("unknown option: %v", err)
	}
	bad = reply
	bad.Kind = "deferred"
	bad.QuestionID = "required"
	bad.OptionIDs = nil
	bad.Text = ""
	if err = f.store.ReplyGoal(ctx, f.owner, bad); !errors.Is(err, workflow.ErrInvalid) {
		t.Fatalf("required deferred: %v", err)
	}
	if err = f.store.ReplyGoal(ctx, f.owner, reply); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ReplyGoal(ctx, f.owner, reply); err != nil {
		t.Fatalf("answer retry: %v", err)
	}
	bad = reply
	bad.OptionIDs = []string{"full"}
	if err = f.store.ReplyGoal(ctx, f.owner, bad); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("reused answer key: %v", err)
	}
	bad.RequestKey = "new-answer"
	if err = f.store.ReplyGoal(ctx, f.owner, bad); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale answer: %v", err)
	}
	bad.ExpectedRevision = 2
	if err = f.store.ReplyGoal(ctx, f.owner, bad); err != nil {
		t.Fatal(err)
	}
	g, entries, _, err = f.store.GetGoal(ctx, f.org, id, 0)
	if err != nil || g.Revision != 3 || len(entries) != 2 || g.Questions[0].Answer.OptionIDs[0] != "full" || entries[0].OptionIDs[0] != "small" {
		t.Fatalf("answer history: %+v %+v %v", g, entries, err)
	}
	// Concurrent tabs have one winner; the losing draft cannot replace it.
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- f.store.ReplyGoal(ctx, f.owner, GoalReply{GoalID: id, RequestKey: fmt.Sprint("concurrent", i), ExpectedRevision: 3, Kind: "message", Text: "Context"})
		}()
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, workflow.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrency: %d %d", successes, conflicts)
	}
	// Pagination cannot hide the current decision, even when reading old history.
	for i := int64(4); i < 58; i++ {
		if err = f.store.ReplyGoal(ctx, f.owner, GoalReply{GoalID: id, RequestKey: fmt.Sprint("message", i), ExpectedRevision: i, Kind: "message", Text: "More context"}); err != nil {
			t.Fatal(err)
		}
	}
	g, entries, more, err = f.store.GetGoal(ctx, f.org, id, 0)
	if err != nil || len(entries) != 50 || !more {
		t.Fatalf("history page: %d %v %v", len(entries), more, err)
	}
	_, older, more, err := f.store.GetGoal(ctx, f.org, id, entries[0].Sequence)
	if err != nil || len(older) != 7 || more || older[0].Sequence != 2 {
		t.Fatalf("older history: %d %v %v", len(older), more, err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE workflow_goal_entries SET body='rewritten' WHERE organization_id=$1 AND goal_id=$2`, f.org, id); err == nil {
		t.Fatal("history was mutable")
	}
	strangerOrg := organization(t, f.pool, "other-goals")
	stranger := caller(t, f.pool, strangerOrg, "owner", "other-owner")
	if _, _, _, err = f.store.GetGoal(ctx, strangerOrg, id, 0); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("cross tenant read: %v", err)
	}
	if err = f.store.ReplyGoal(ctx, stranger, GoalReply{GoalID: id, RequestKey: "foreign", ExpectedRevision: g.Revision, Kind: "message", Text: "No"}); !errors.Is(err, workflow.ErrNotFound) {
		t.Fatalf("cross tenant write: %v", err)
	}
	viewer := caller(t, f.pool, f.org, "viewer", "goal-viewer")
	if _, err = f.store.CreateGoal(ctx, viewer, in); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer create: %v", err)
	}
	if err = f.store.ReplyGoal(ctx, viewer, GoalReply{GoalID: id, RequestKey: "viewer", ExpectedRevision: g.Revision, Kind: "message", Text: "No"}); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer write: %v", err)
	}
	if _, err = f.pool.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp() WHERE id=$1`, f.owner.SessionID); err != nil {
		t.Fatal(err)
	}
	if err = f.store.ReplyGoal(ctx, f.owner, GoalReply{GoalID: id, RequestKey: "revoked", ExpectedRevision: g.Revision, Kind: "message", Text: "No"}); !errors.Is(err, workflow.ErrFenced) {
		t.Fatalf("revoked session: %v", err)
	}
}
