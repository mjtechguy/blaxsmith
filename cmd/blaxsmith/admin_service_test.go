package main

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestAdminErrorCodes(t *testing.T) {
	for err, want := range map[error]connect.Code{
		workflow.ErrAdminDenied:              connect.CodePermissionDenied,
		workflow.ErrProjectModelAccessDenied: connect.CodePermissionDenied,
		workflow.ErrNotFound:                 connect.CodeNotFound,
		workflow.ErrFenced:                   connect.CodeUnauthenticated,
	} {
		if got := connect.CodeOf(adminError(err)); got != want {
			t.Fatalf("%v mapped to %v, want %v", err, got, want)
		}
	}
}

// testAdminBrowserAPI checks the AdminService through the real HTTPS handler
// for the session's current role. Non-admins are denied every RPC.
func testAdminBrowserAPI(t *testing.T, ctx context.Context, client *http.Client, origin, csrf string, admin bool) {
	t.Helper()
	c := apiv1connect.NewAdminServiceClient(client, origin+"/api")
	overview := connect.NewRequest(&api.GetAdminOverviewRequest{})
	overview.Header().Set("Origin", origin)
	got, err := c.GetAdminOverview(ctx, overview)
	audit := connect.NewRequest(&api.ListAuditEventsRequest{PageSize: 5})
	audit.Header().Set("Origin", origin)
	events, auditErr := c.ListAuditEvents(ctx, audit)
	if !admin {
		if connect.CodeOf(err) != connect.CodePermissionDenied || connect.CodeOf(auditErr) != connect.CodePermissionDenied {
			t.Fatalf("non-admin read admin dashboard: %v, %v", err, auditErr)
		}
		halt := connect.NewRequest(&api.HaltRunRequest{RunId: "00000000-0000-0000-0000-000000000001"})
		halt.Header().Set("Origin", origin)
		halt.Header().Set("X-Blaxsmith-CSRF", csrf)
		if _, err := c.HaltRun(ctx, halt); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin halted run: %v", err)
		}
		return
	}
	if err != nil || got.Msg.Capacity == nil || len(got.Msg.RunStates) != 6 || got.Msg.GeneratedAt == "" {
		t.Fatalf("owner overview: %+v, %v", got, err)
	}
	if auditErr != nil || len(events.Msg.Events) == 0 {
		t.Fatalf("owner audit: %+v, %v", events, auditErr)
	}
	if events.Msg.NextPageToken != "" {
		audit.Msg.PageToken = events.Msg.NextPageToken
		if _, err := c.ListAuditEvents(ctx, audit); err != nil {
			t.Fatalf("audit next page: %v", err)
		}
	}
	audit.Msg.PageToken = "bm90LWFuLWlk"
	if _, err := c.ListAuditEvents(ctx, audit); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad audit token: %v", err)
	}
	revoke := connect.NewRequest(&api.RevokeGrantRequest{GrantId: "00000000-0000-0000-0000-000000000001"})
	revoke.Header().Set("Origin", origin)
	if _, err := c.RevokeGrant(ctx, revoke); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("revoke without CSRF: %v", err)
	}
	revoke.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := c.RevokeGrant(ctx, revoke); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("revoke unknown grant: %v", err)
	}
}
