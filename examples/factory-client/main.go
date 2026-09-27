// External factory intake example. No runtime or provider access is needed.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	origin := flag.String("server", "", "Exact Blaxsmith HTTPS origin")
	key := flag.String("request-key", "", "Stable key for this goal and initial plan")
	planPath := flag.String("plan", "", "example-factory/plan/v1 JSON file")
	flag.Parse()
	u, err := url.Parse(*origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != *origin {
		return errors.New("server must be an exact HTTPS origin")
	}
	token := os.Getenv("BLAXSMITH_API_TOKEN")
	if token == "" || len(*key) < 1 || len(*key) > 120 || *planPath == "" || flag.NArg() != 0 {
		return errors.New("set BLAXSMITH_API_TOKEN and supply --server, --request-key and --plan")
	}
	file, err := os.Open(*planPath)
	if err != nil {
		return errors.New("cannot open plan file")
	}
	defer file.Close()
	plan, err := io.ReadAll(io.LimitReader(file, (256<<10)+1))
	var header struct {
		Schema string `json:"schema_version"`
	}
	if err != nil || len(plan) > 256<<10 || json.Unmarshal(plan, &header) != nil || header.Schema != "example-factory/plan/v1" {
		return errors.New("plan must be example-factory/plan/v1 JSON, at most 256 KiB")
	}
	client := apiv1connect.NewWorkflowServiceClient(&http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, *origin+"/machine", connect.WithReadMaxBytes(4<<20), connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	access, err := client.DescribeMachineAccess(ctx, connect.NewRequest(&api.DescribeMachineAccessRequest{}))
	if err != nil {
		return fmt.Errorf("access discovery: %s", connect.CodeOf(err))
	}
	credential := access.Msg.GetCredential()
	if !slices.Contains(credential.GetScopes(), "project.read") || !slices.Contains(credential.GetScopes(), "goal.write") {
		return errors.New("credential requires project.read and goal.write")
	}
	capabilities, err := client.GetPlatformCapabilities(ctx, connect.NewRequest(&api.GetPlatformCapabilitiesRequest{}))
	if err != nil || !capabilities.Msg.GetMachineApi() {
		return errors.New("machine capability discovery failed")
	}
	goal, err := client.CreateGoal(ctx, connect.NewRequest(&api.CreateGoalRequest{
		ProjectId: credential.GetProjectId(), RequestKey: *key + "/goal",
		Title: "External factory example", Brief: "Inspect the repository and record a plan before implementation.",
		FactoryId: "example-factory", FactoryVersion: "1",
	}))
	if err != nil {
		return fmt.Errorf("create goal: %s; reconcile the request key before retrying", connect.CodeOf(err))
	}
	// Emit the committed ID before the next mutation, including on a partial failure.
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"goal_id": goal.Msg.Goal.Id, "state": "goal_recorded"}); err != nil {
		return err
	}
	version, err := client.SaveGoalPlan(ctx, connect.NewRequest(&api.SaveGoalPlanRequest{
		GoalId: goal.Msg.Goal.Id, RequestKey: *key + "/plan", ExpectedGoalRevision: 1, ExpectedPlanVersion: 0, ContentJson: string(plan),
	}))
	if err != nil {
		return fmt.Errorf("save plan: %s; inspect the recorded goal before retrying", connect.CodeOf(err))
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"goal_id": goal.Msg.Goal.Id, "plan_version": version.Msg.Version, "state": "plan_recorded"})
}
