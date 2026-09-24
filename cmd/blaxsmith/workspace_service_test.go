package main

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
)

// testWorkspaceBrowserAPI checks the WorkspaceService through the real HTTPS
// handler for the session's current role. Every role reads Home, Inbox, and
// runs; only owners and admins page the member directory.
func testWorkspaceBrowserAPI(t *testing.T, ctx context.Context, client *http.Client, origin string, admin bool) {
	t.Helper()
	c := apiv1connect.NewWorkspaceServiceClient(client, origin+"/api")
	home := connect.NewRequest(&api.GetWorkspaceHomeRequest{})
	home.Header().Set("Origin", origin)
	got, err := c.GetWorkspaceHome(ctx, home)
	if err != nil || got.Msg.GeneratedAt == "" || len(got.Msg.RecentRuns) == 0 || got.Msg.RecentRuns[0].ProjectName == "" {
		t.Fatalf("workspace home: %+v, %v", got, err)
	}
	if !admin && (got.Msg.WaitingOnYou != 0 || len(got.Msg.Waiting) != 0) {
		t.Fatalf("viewer has actionable items: %+v", got.Msg)
	}
	inbox := connect.NewRequest(&api.ListInboxRequest{ActionableOnly: true})
	inbox.Header().Set("Origin", origin)
	items, err := c.ListInbox(ctx, inbox)
	if err != nil || (!admin && (items.Msg.TotalCount != 0 || len(items.Msg.Items) != 0)) {
		t.Fatalf("workspace inbox: %+v, %v", items, err)
	}
	inbox.Msg.Page = -1
	if _, err := c.ListInbox(ctx, inbox); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid inbox page: %v", err)
	}
	runs := connect.NewRequest(&api.ListWorkspaceRunsRequest{PageSize: 1, SortBy: "launch_key", SortDirection: "asc"})
	runs.Header().Set("Origin", origin)
	page, err := c.ListWorkspaceRuns(ctx, runs)
	if err != nil || len(page.Msg.Runs) != 1 || page.Msg.TotalCount < 1 || page.Msg.Runs[0].CreatedAt == "" {
		t.Fatalf("workspace runs: %+v, %v", page, err)
	}
	runs.Msg.SortBy = "name"
	if _, err := c.ListWorkspaceRuns(ctx, runs); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid run sort: %v", err)
	}
	members := connect.NewRequest(&api.ListMembersPageRequest{PageSize: 10})
	members.Header().Set("Origin", origin)
	directory, err := c.ListMembersPage(ctx, members)
	if !admin {
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin paged members: %v", err)
		}
		return
	}
	if err != nil || directory.Msg.TotalCount < 1 || len(directory.Msg.Members) < 1 || directory.Msg.Members[0].Role != "owner" {
		t.Fatalf("owner member page: %+v, %v", directory, err)
	}
	members.Msg.Statuses = []string{"pending"}
	if _, err := c.ListMembersPage(ctx, members); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid member status: %v", err)
	}
}
