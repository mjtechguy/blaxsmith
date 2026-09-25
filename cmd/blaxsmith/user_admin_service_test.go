package main

import (
	"context"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func TestUserAdminErrorCodes(t *testing.T) {
	for err, want := range map[error]connect.Code{
		identity.ErrUserAdminDenied: connect.CodePermissionDenied,
		identity.ErrOwnerOnly:       connect.CodePermissionDenied,
		identity.ErrSharedAccount:   connect.CodePermissionDenied,
		identity.ErrLastOwner:       connect.CodeFailedPrecondition,
		identity.ErrSelfDisable:     connect.CodeFailedPrecondition,
		identity.ErrUserExists:      connect.CodeAlreadyExists,
		identity.ErrUserNotFound:    connect.CodeNotFound,
		identity.ErrLinkInvalid:     connect.CodeNotFound,
		identity.ErrPassword:        connect.CodeInvalidArgument,
		identity.ErrUnauthenticated: connect.CodeUnauthenticated,
		identity.ErrRateLimited:     connect.CodeResourceExhausted,
	} {
		if got := connect.CodeOf(userAdminError(err)); got != want {
			t.Fatalf("%v mapped to %v, want %v", err, got, want)
		}
	}
}

// testUsersBrowserAPI checks UserAdminService through the real HTTPS handler:
// role gating, CSRF on mutations and on public link completion, and
// single-use setup links.
func testUsersBrowserAPI(t *testing.T, ctx context.Context, client *http.Client, origin, csrf, self string, admin bool) {
	t.Helper()
	c := apiv1connect.NewUserAdminServiceClient(client, origin+"/api")
	list := connect.NewRequest(&api.ListOrgMembersRequest{})
	list.Header().Set("Origin", origin)
	members, err := c.ListOrgMembers(ctx, list)
	invite := connect.NewRequest(&api.InviteUserRequest{Email: "Browser-Invitee@Example.com", DisplayName: "Browser Invitee", Role: "member"})
	invite.Header().Set("Origin", origin)
	invite.Header().Set("X-Blaxsmith-CSRF", csrf)
	if !admin {
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin listed members: %v", err)
		}
		invite.Msg.Email = "browser-denied@example.com"
		if _, err := c.InviteUser(ctx, invite); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("non-admin invited: %v", err)
		}
		return
	}
	if err != nil || len(members.Msg.Members) == 0 {
		t.Fatalf("owner listed members: %+v %v", members, err)
	}
	noCSRF := connect.NewRequest(invite.Msg)
	noCSRF.Header().Set("Origin", origin)
	if _, err := c.InviteUser(ctx, noCSRF); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("invite without CSRF: %v", err)
	}
	invited, err := c.InviteUser(ctx, invite)
	if err != nil || invited.Msg.Link.GetToken() == "" || invited.Msg.Link.GetPurpose() != "setup" {
		t.Fatalf("invite: %+v %v", invited, err)
	}
	token := invited.Msg.Link.Token
	get := connect.NewRequest(&api.GetAccountLinkRequest{Token: token})
	if _, err := c.GetAccountLink(ctx, get); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("link read without origin: %v", err)
	}
	get.Header().Set("Origin", origin)
	if got, err := c.GetAccountLink(ctx, get); err != nil || got.Msg.Username != "browser-invitee" {
		t.Fatalf("link read: %+v %v", got, err)
	}
	complete := connect.NewRequest(&api.CompleteAccountLinkRequest{Token: token, Password: "browser invitee password"})
	complete.Header().Set("Origin", origin)
	if _, err := c.CompleteAccountLink(ctx, complete); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("complete without CSRF: %v", err)
	}
	complete.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := c.CompleteAccountLink(ctx, complete); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if _, err := c.CompleteAccountLink(ctx, complete); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("link reused: %v", err)
	}
	demote := connect.NewRequest(&api.SetUserRoleRequest{PrincipalId: self, Role: "admin"})
	demote.Header().Set("Origin", origin)
	demote.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := c.SetUserRole(ctx, demote); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("last owner demoted: %v", err)
	}
}
