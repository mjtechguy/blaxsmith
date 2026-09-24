package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestInteractionErrorAndWireShape(t *testing.T) {
	for err, want := range map[error]connect.Code{
		interact.ErrDenied:   connect.CodePermissionDenied,
		workflow.ErrConflict: connect.CodeFailedPrecondition,
		workflow.ErrInvalid:  connect.CodeInvalidArgument,
		workflow.ErrNotFound: connect.CodeNotFound,
		workflow.ErrFenced:   connect.CodeUnauthenticated,
	} {
		if got := connect.CodeOf(interactionError(err)); got != want {
			t.Fatalf("%v mapped to %v, want %v", err, got, want)
		}
	}
	message := interactionMessage(interact.Record{ID: "i", AttemptID: "a", Stage: "s", State: "answered",
		Interaction: interact.Interaction{Kind: "question", Title: "t", BodyMD: "b", MultiSelect: true, AllowFreeText: true, Blocking: true,
			Options: []interact.Option{{ID: "o", Label: "l", Description: "d", Recommended: true}},
			Sources: []interact.Source{{Path: "p", Line: 2}}, Interview: &interact.Interview{Round: 1, FinalizeOption: "o"}},
		Answer: &interact.Answer{OptionIDs: []string{"o"}, Text: "x", AnsweredBy: "u"}})
	data, err := protojson.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	for _, key := range []string{"id", "attemptId", "stage", "kind", "title", "bodyMd", "options", "multiSelect",
		"allowFreeText", "blocking", "sources", "interview", "state", "answer", "createdAt"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("wire shape missing %s: %s", key, data)
		}
	}
	for _, want := range []string{`"finalizeOption":"o"`, `"optionIds":["o"]`, `"answeredBy":"u"`, `"recommended":true`} {
		if !strings.Contains(strings.ReplaceAll(string(data), " ", ""), want) {
			t.Fatalf("wire shape missing %s: %s", want, data)
		}
	}
}

// testInteractionBrowserAPI drives the interaction RPCs and the existing run
// activity stream through the real HTTPS app handler.
func testInteractionBrowserAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *http.Client,
	origin, csrf string, owner identity.FirstOwner) (string, string) {
	t.Helper()
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, owner.OrganizationID, "interaction-project", "Interactions")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: owner.OrganizationID, ProjectID: project,
		LaunchKey: "interactions", SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64),
		VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, owner.OrganizationID, run.ID, "interview", strings.Repeat("b", 64), 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`,
		owner.OrganizationID, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, owner.OrganizationID, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	ix, err := interact.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range []string{
		`{"seq":1,"interaction":{"id":"q1","kind":"interview_round","title":"Scope?","options":[{"id":"a","label":"Small"},{"id":"done","label":"Done"}],"allow_free_text":false,"interview":{"round":1,"finalize_option":"done"}}}`,
		`{"seq":2,"event":{"type":"cycle","cycle":2,"max_cycles":3}}`,
	} {
		if err := ix.Persist(ctx, attempt, []byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w := apiv1connect.NewWorkflowServiceClient(client, origin+"/api")
	list := connect.NewRequest(&api.ListInteractionsRequest{RunId: run.ID})
	list.Header().Set("Origin", origin)
	listed, err := w.ListInteractions(ctx, list)
	if err != nil || len(listed.Msg.Interactions) != 1 || listed.Msg.Interactions[0].Stage != "interview" ||
		listed.Msg.Interactions[0].State != "open" || listed.Msg.Interactions[0].Interview.FinalizeOption != "done" {
		t.Fatalf("list interactions: %+v, %v", listed, err)
	}
	id := listed.Msg.Interactions[0].Id
	answer := func(options []string, text string, withCSRF bool) error {
		req := connect.NewRequest(&api.AnswerInteractionRequest{InteractionId: id, OptionIds: options, Text: text})
		req.Header().Set("Origin", origin)
		if withCSRF {
			req.Header().Set("X-Blaxsmith-CSRF", csrf)
		}
		_, err := w.AnswerInteraction(ctx, req)
		return err
	}
	if err := answer([]string{"a"}, "", false); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("answer without CSRF: %v", err)
	}
	if err := answer([]string{"a"}, "free text not allowed", true); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("free text accepted: %v", err)
	}
	if err := answer([]string{"zzz"}, "", true); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown option accepted: %v", err)
	}
	if err := answer([]string{"a"}, "", true); err != nil {
		t.Fatal(err)
	}
	if err := answer([]string{"done"}, "", true); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("second answer: %v", err)
	}
	steer := connect.NewRequest(&api.SteerAttemptRequest{AttemptId: attempt.ID, Kind: "halt", Reason: "enough"})
	steer.Header().Set("Origin", origin)
	steer.Header().Set("X-Blaxsmith-CSRF", csrf)
	if got, err := w.SteerAttempt(ctx, steer); err != nil || got.Msg.SteerId == "" {
		t.Fatalf("steer: %+v, %v", got, err)
	}
	steer.Msg.Kind = "reboot"
	if _, err := w.SteerAttempt(ctx, steer); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown steer kind: %v", err)
	}
	// Replay from a cursor on the existing activity stream carries the new kinds.
	opened := ""
	for _, e := range readActivityFrames(t, ctx, client, origin, run.ID, "0", 50) {
		if e["kind"] == "interaction.opened" {
			opened = e["id"].(string)
		}
	}
	tail := readActivityFrames(t, ctx, client, origin, run.ID, opened, 10)
	var kinds []string
	for _, e := range tail {
		kinds = append(kinds, e["kind"].(string))
	}
	if opened == "" || strings.Join(kinds, ",") != "attempt.progress,interaction.answered,attempt.control" {
		t.Fatalf("stream replay after %q: %v", opened, kinds)
	}
	payload, _ := tail[0]["payloadJson"].(string)
	if !strings.Contains(strings.ReplaceAll(payload, " ", ""), `"max_cycles":3`) || tail[1]["payloadJson"] != nil {
		t.Fatalf("progress payload frames: %v", tail)
	}
	return id, attempt.ID
}

// readActivityFrames reads up to n data frames (fewer if the stream idles).
func readActivityFrames(t *testing.T, ctx context.Context, client *http.Client, origin, runID, after string, n int) []map[string]any {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	req.Header.Set("Last-Event-ID", after)
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("activity stream: HTTP %d", res.StatusCode)
	}
	frames := make(chan map[string]any)
	go func() {
		defer close(frames)
		reader := bufio.NewReader(res.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			if data, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), "data: "); ok {
				var frame map[string]any
				if json.Unmarshal([]byte(data), &frame) == nil {
					select {
					case frames <- frame:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	}()
	var out []map[string]any
	idle := time.After(1500 * time.Millisecond)
	for len(out) < n {
		select {
		case frame, ok := <-frames:
			if !ok {
				return out
			}
			out = append(out, frame)
		case <-idle:
			return out
		}
	}
	return out
}
