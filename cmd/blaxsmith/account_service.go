package main

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

// accountService serves the caller's own account. It never takes a
// principal id: everything acts on the live session's principal. Only
// GetMyProfile and UpdateMyProfile accept an email_required session, so a
// one-time legacy login can set its email and nothing else.
type accountService struct {
	guard   *identity.BrowserGuard
	manager *identity.SessionManager
}

func accountError(err error) error {
	switch {
	case errors.Is(err, identity.ErrRateLimited):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many attempts; try again soon"))
	case errors.Is(err, identity.ErrCurrentPassword):
		return connect.NewError(connect.CodePermissionDenied, err)
	case errors.Is(err, identity.ErrEmailTaken):
		return connect.NewError(connect.CodeAlreadyExists, err)
	case errors.Is(err, identity.ErrEmailRequired), errors.Is(err, identity.ErrCurrentSession):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, identity.ErrSessionNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, identity.ErrEmailInvalid), errors.Is(err, identity.ErrUserInvalid), errors.Is(err, identity.ErrPassword):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, identity.ErrUnauthenticated):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("account unavailable"))
	}
}

func myProfile(p identity.Profile) *api.MyProfile {
	return &api.MyProfile{PrincipalId: p.PrincipalID, Email: p.Email, EmailVerified: p.EmailVerified,
		DisplayName: p.DisplayName, Handle: p.Handle, OrganizationId: p.OrganizationID,
		OrganizationSlug: p.OrganizationSlug, OrganizationName: p.OrganizationName, Role: p.Role,
		CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339), EmailRequired: p.EmailRequired}
}

func (s *accountService) GetMyProfile(ctx context.Context, req *connect.Request[api.GetMyProfileRequest]) (*connect.Response[api.GetMyProfileResponse], error) {
	caller, err := s.guard.AccountCaller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	profile, err := s.manager.Profile(ctx, caller)
	if err != nil {
		return nil, accountError(err)
	}
	return connect.NewResponse(&api.GetMyProfileResponse{Profile: myProfile(profile)}), nil
}

func (s *accountService) UpdateMyProfile(ctx context.Context, req *connect.Request[api.UpdateMyProfileRequest]) (*connect.Response[api.UpdateMyProfileResponse], error) {
	caller, err := s.guard.AccountCaller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	password := []byte(req.Msg.CurrentPassword)
	defer clear(password)
	profile, revoked, err := s.manager.UpdateProfile(ctx, caller, identity.ProfileChange{
		DisplayName: req.Msg.DisplayName, Email: req.Msg.Email, CurrentPassword: password})
	if err != nil {
		return nil, accountError(err)
	}
	return connect.NewResponse(&api.UpdateMyProfileResponse{Profile: myProfile(profile), RevokedSessions: revoked}), nil
}

func (s *accountService) ChangeMyPassword(ctx context.Context, req *connect.Request[api.ChangeMyPasswordRequest]) (*connect.Response[api.ChangeMyPasswordResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	current, next := []byte(req.Msg.CurrentPassword), []byte(req.Msg.NewPassword)
	defer clear(current)
	defer clear(next)
	revoked, err := s.manager.ChangePassword(ctx, caller, current, next)
	if err != nil {
		return nil, accountError(err)
	}
	return connect.NewResponse(&api.ChangeMyPasswordResponse{RevokedSessions: revoked}), nil
}

func (s *accountService) ListMySessions(ctx context.Context, req *connect.Request[api.ListMySessionsRequest]) (*connect.Response[api.ListMySessionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), false)
	if err != nil {
		return nil, err
	}
	sessions, err := s.manager.ListSessions(ctx, caller)
	if err != nil {
		return nil, accountError(err)
	}
	response := &api.ListMySessionsResponse{}
	for _, session := range sessions {
		response.Sessions = append(response.Sessions, &api.MySession{Id: session.ID, Current: session.Current,
			OrganizationSlug: session.OrganizationSlug, CreatedAt: adminTime(session.CreatedAt),
			LastSeenAt: adminOptionalTime(session.LastSeen), ExpiresAt: adminTime(session.ExpiresAt),
			SourceAddress: session.SourceAddress, UserAgent: session.UserAgent})
	}
	return connect.NewResponse(response), nil
}

func (s *accountService) RevokeMySession(ctx context.Context, req *connect.Request[api.RevokeMySessionRequest]) (*connect.Response[api.RevokeMySessionResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	if err := s.manager.RevokeSession(ctx, caller, req.Msg.SessionId); err != nil {
		return nil, accountError(err)
	}
	return connect.NewResponse(&api.RevokeMySessionResponse{}), nil
}

func (s *accountService) RevokeMyOtherSessions(ctx context.Context, req *connect.Request[api.RevokeMyOtherSessionsRequest]) (*connect.Response[api.RevokeMyOtherSessionsResponse], error) {
	caller, err := s.guard.Caller(ctx, req.Header(), true)
	if err != nil {
		return nil, err
	}
	revoked, err := s.manager.RevokeOtherSessions(ctx, caller)
	if err != nil {
		return nil, accountError(err)
	}
	return connect.NewResponse(&api.RevokeMyOtherSessionsResponse{Revoked: revoked}), nil
}
