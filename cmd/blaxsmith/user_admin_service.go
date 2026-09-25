package main

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// userAdminService serves member management to owners and admins and the
// public account-link endpoints. Reads check origin and a live session;
// mutations also require CSRF, and the store rechecks role under lock.
type userAdminService struct {
	guard *identity.BrowserGuard
	users *identity.UserAdmin
}

func userAdminError(err error) error {
	switch {
	case errors.Is(err, identity.ErrUserAdminDenied):
		return connect.NewError(connect.CodePermissionDenied, errors.New("organization administration denied"))
	case errors.Is(err, identity.ErrOwnerOnly), errors.Is(err, identity.ErrSharedAccount):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, identity.ErrLastOwner), errors.Is(err, identity.ErrSelfDisable):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, identity.ErrUserExists), errors.Is(err, identity.ErrEmailTaken):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, identity.ErrUserNotFound), errors.Is(err, identity.ErrLinkInvalid):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, identity.ErrUserInvalid), errors.Is(err, identity.ErrPassword), errors.Is(err, identity.ErrEmailInvalid):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, identity.ErrUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
	case errors.Is(err, identity.ErrRateLimited):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many attempts; try again soon"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("user administration unavailable"))
	}
}

func accountLink(link identity.AccountLink) *api.AccountLink {
	return &api.AccountLink{Token: link.Token, Purpose: link.Purpose, ExpiresAt: adminTime(link.ExpiresAt)}
}

func (s *userAdminService) ListOrgMembers(ctx context.Context, req *connect.Request[api.ListOrgMembersRequest]) (*connect.Response[api.ListOrgMembersResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	members, err := s.users.ListMembers(ctx, caller)
	if err != nil {
		return nil, userAdminError(err)
	}
	response := &api.ListOrgMembersResponse{}
	for _, m := range members {
		response.Members = append(response.Members, orgMember(m))
	}
	return connect.NewResponse(response), nil
}

func (s *userAdminService) InviteUser(ctx context.Context, req *connect.Request[api.InviteUserRequest]) (*connect.Response[api.InviteUserResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	link, err := s.users.Invite(ctx, caller, req.Msg.Email, req.Msg.DisplayName, req.Msg.Role)
	if err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.InviteUserResponse{PrincipalId: link.PrincipalID, Link: accountLink(link)}), nil
}

func (s *userAdminService) SetUserRole(ctx context.Context, req *connect.Request[api.SetUserRoleRequest]) (*connect.Response[api.SetUserRoleResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.users.SetRole(ctx, caller, req.Msg.PrincipalId, req.Msg.Role); err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.SetUserRoleResponse{}), nil
}

func (s *userAdminService) SetUserEnabled(ctx context.Context, req *connect.Request[api.SetUserEnabledRequest]) (*connect.Response[api.SetUserEnabledResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.users.SetEnabled(ctx, caller, req.Msg.PrincipalId, req.Msg.Enabled); err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.SetUserEnabledResponse{}), nil
}

func (s *userAdminService) IssueResetLink(ctx context.Context, req *connect.Request[api.IssueResetLinkRequest]) (*connect.Response[api.IssueResetLinkResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	link, err := s.users.IssueReset(ctx, caller, req.Msg.PrincipalId)
	if err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.IssueResetLinkResponse{Link: accountLink(link)}), nil
}

func (s *userAdminService) RevokeUserSessions(ctx context.Context, req *connect.Request[api.RevokeUserSessionsRequest]) (*connect.Response[api.RevokeUserSessionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	count, err := s.users.RevokeSessions(ctx, caller, req.Msg.PrincipalId)
	if err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.RevokeUserSessionsResponse{Revoked: count}), nil
}

func (s *userAdminService) SetUserEmail(ctx context.Context, req *connect.Request[api.SetUserEmailRequest]) (*connect.Response[api.SetUserEmailResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.users.SetEmail(ctx, caller, req.Msg.PrincipalId, req.Msg.Email); err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.SetUserEmailResponse{}), nil
}

func (s *userAdminService) GetAccountLink(ctx context.Context, req *connect.Request[api.GetAccountLinkRequest]) (*connect.Response[api.GetAccountLinkResponse], error) {
	if err := s.guard.CheckRequest(req.Header(), false); err != nil {
		return nil, err
	}
	if err := s.users.AllowLink(ctx, req.Msg.Token, req.Peer().Addr); err != nil {
		return nil, userAdminError(err)
	}
	info, err := s.users.InspectLink(ctx, req.Msg.Token)
	if err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.GetAccountLinkResponse{Purpose: info.Purpose, Username: info.Username,
		DisplayName: info.DisplayName, OrganizationSlug: info.OrganizationSlug, OrganizationName: info.OrganizationName,
		ExpiresAt: adminTime(info.ExpiresAt), Email: info.Email}), nil
}

func (s *userAdminService) CompleteAccountLink(ctx context.Context, req *connect.Request[api.CompleteAccountLinkRequest]) (*connect.Response[api.CompleteAccountLinkResponse], error) {
	if err := s.guard.CheckRequest(req.Header(), true); err != nil {
		return nil, err
	}
	if err := s.users.AllowLink(ctx, req.Msg.Token, req.Peer().Addr); err != nil {
		return nil, userAdminError(err)
	}
	password := []byte(req.Msg.Password)
	defer clear(password)
	info, err := s.users.CompleteLink(ctx, req.Msg.Token, password, req.Msg.DisplayName, req.Msg.Email)
	if err != nil {
		return nil, userAdminError(err)
	}
	return connect.NewResponse(&api.CompleteAccountLinkResponse{OrganizationSlug: info.OrganizationSlug, Email: info.Email}), nil
}

func orgMember(m identity.Member) *api.OrgMember {
	return &api.OrgMember{PrincipalId: m.PrincipalID, Username: m.Username, Email: m.Email, EmailVerified: m.EmailVerified,
		DisplayName: m.DisplayName, Role: m.Role, Status: m.Status, ActiveSessions: m.ActiveSessions,
		LastLoginAt: adminOptionalTime(m.LastLogin), CreatedAt: adminTime(m.CreatedAt)}
}
