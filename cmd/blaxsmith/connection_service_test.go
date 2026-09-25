package main

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
)

// testGitHubConnectGate checks StartGitHubConnect refuses a caller who could
// not create the project connection before any OAuth state exists. No GitHub
// App is registered, so an allowed caller gets FailedPrecondition instead.
func testGitHubConnectGate(t *testing.T, ctx context.Context, client *http.Client, origin, csrf, projectID string, admin bool) {
	t.Helper()
	c := apiv1connect.NewConnectionServiceClient(client, origin+"/api")
	start := connect.NewRequest(&api.StartGitHubConnectRequest{Scope: "project", ProjectId: projectID, ReturnTo: "/"})
	start.Header().Set("Origin", origin)
	start.Header().Set("X-Blaxsmith-CSRF", csrf)
	want := connect.CodePermissionDenied
	if admin {
		want = connect.CodeFailedPrecondition
	}
	if _, err := c.StartGitHubConnect(ctx, start); connect.CodeOf(err) != want {
		t.Fatalf("start GitHub connect (project admin %v): %v", admin, err)
	}
}
