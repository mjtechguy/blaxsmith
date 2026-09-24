package identity

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
)

const accessCookie = "__Host-blaxsmith_access"
const refreshCookie = "__Host-blaxsmith_refresh"
const csrfCookie = "__Host-blaxsmith_csrf"

// browserService is exposed only through NewBrowserHandler's HTTPS/host gate.
// Its peer address must be the real client; a trusted proxy integration must
// resolve client IPs before this handler is used behind a shared ingress.
type browserService struct {
	manager *SessionManager
	origin  string
}

type BrowserGuard struct {
	manager *SessionManager
	origin  string
	host    string
}

func NewBrowserGuard(manager *SessionManager, origin string) (*BrowserGuard, error) {
	u, err := url.Parse(origin)
	if manager == nil || manager.db == nil || manager.limits == nil || err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != origin {
		return nil, errors.New("browser authentication requires an exact HTTPS origin")
	}
	return &BrowserGuard{manager: manager, origin: origin, host: u.Host}, nil
}

func (g *BrowserGuard) Wrap(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.TLS == nil || !strings.EqualFold(r.Host, g.host) {
			http.Error(w, "secure origin required", http.StatusForbidden)
			return
		}
		handler.ServeHTTP(w, r)
	})
}

// Caller requires a live membership at the time of each browser request.
func (g *BrowserGuard) Caller(ctx context.Context, header http.Header, mutation bool) (Caller, error) {
	s := browserService{manager: g.manager, origin: g.origin}
	var err error
	if mutation {
		err = s.checkCSRF(header)
	} else {
		err = s.checkOrigin(header)
	}
	if err != nil {
		return Caller{}, err
	}
	caller, err := g.manager.ValidateAccess(ctx, cookieValue(header, accessCookie))
	if err != nil {
		return Caller{}, browserAuthError(err)
	}
	return caller, nil
}

// CheckRequest applies the same origin (and, for mutations, CSRF) checks as
// Caller without requiring a session, for public account-link endpoints.
func (g *BrowserGuard) CheckRequest(header http.Header, mutation bool) error {
	s := browserService{manager: g.manager, origin: g.origin}
	if mutation {
		return s.checkCSRF(header)
	}
	return s.checkOrigin(header)
}

// StreamCaller accepts the headers native same-origin EventSource sends. It
// cannot set the custom Origin header required by Connect read requests.
func (g *BrowserGuard) StreamCaller(ctx context.Context, header http.Header) (Caller, error) {
	origin, site := header.Get("Origin"), header.Get("Sec-Fetch-Site")
	if (origin == "" && site == "") || (origin != "" && origin != g.origin) ||
		(site != "" && site != "same-origin") {
		return Caller{}, connect.NewError(connect.CodePermissionDenied, errors.New("request origin denied"))
	}
	caller, err := g.manager.ValidateAccess(ctx, cookieValue(header, accessCookie))
	if err != nil {
		return Caller{}, browserAuthError(err)
	}
	return caller, nil
}

func NewBrowserHandler(manager *SessionManager, origin string) (string, http.Handler, error) {
	guard, err := NewBrowserGuard(manager, origin)
	if err != nil {
		return "", nil, err
	}
	path, handler := apiv1connect.NewAuthServiceHandler(&browserService{manager: manager, origin: origin},
		connect.WithReadMaxBytes(4096))
	return path, guard.Wrap(handler), nil
}

func (s *browserService) GetCsrf(_ context.Context, req *connect.Request[api.GetCsrfRequest]) (*connect.Response[api.GetCsrfResponse], error) {
	if err := s.checkOrigin(req.Header()); err != nil {
		return nil, err
	}
	token := cookieValue(req.Header(), csrfCookie)
	if !validCSRFToken(token) {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, connect.NewError(connect.CodeInternal, errors.New("csrf token unavailable"))
		}
		token = base64.RawURLEncoding.EncodeToString(secret[:])
		clear(secret[:])
	}
	response := connect.NewResponse(&api.GetCsrfResponse{Token: token})
	response.Header().Add("Set-Cookie", browserCookie(csrfCookie, token, 3600).String())
	response.Header().Set("Cache-Control", "no-store")
	return response, nil
}

func (s *browserService) LoginLocal(ctx context.Context, req *connect.Request[api.LoginLocalRequest]) (*connect.Response[api.LoginLocalResponse], error) {
	if err := s.checkCSRF(req.Header()); err != nil {
		return nil, err
	}
	source, err := peerAddress(req.Peer().Addr)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, errors.New("client address unavailable"))
	}
	password := []byte(req.Msg.Password)
	defer clear(password)
	tokens, err := s.manager.LoginLocal(ctx, req.Msg.OrganizationSlug, req.Msg.Username, password, source)
	if err != nil {
		return nil, browserAuthError(err)
	}
	response := connect.NewResponse(&api.LoginLocalResponse{Session: tokenIdentity(tokens)})
	setSessionCookies(response.Header(), tokens)
	return response, nil
}

func (s *browserService) RefreshSession(ctx context.Context, req *connect.Request[api.RefreshSessionRequest]) (*connect.Response[api.RefreshSessionResponse], error) {
	if err := s.checkCSRF(req.Header()); err != nil {
		return nil, err
	}
	tokens, err := s.manager.Refresh(ctx, cookieValue(req.Header(), refreshCookie))
	if err != nil {
		result := browserAuthError(err)
		if connect.CodeOf(result) == connect.CodeUnauthenticated {
			clearSessionCookies(result.Meta())
		}
		return nil, result
	}
	response := connect.NewResponse(&api.RefreshSessionResponse{Session: tokenIdentity(tokens)})
	setSessionCookies(response.Header(), tokens)
	return response, nil
}

func (s *browserService) CurrentSession(ctx context.Context, req *connect.Request[api.CurrentSessionRequest]) (*connect.Response[api.CurrentSessionResponse], error) {
	if err := s.checkOrigin(req.Header()); err != nil {
		return nil, err
	}
	caller, err := s.manager.ValidateAccess(ctx, cookieValue(req.Header(), accessCookie))
	if err != nil {
		return nil, browserAuthError(err)
	}
	response := connect.NewResponse(&api.CurrentSessionResponse{Session: &api.SessionIdentity{
		OrganizationId: caller.OrganizationID, PrincipalId: caller.PrincipalID,
		Role: caller.Role, AccessExpiresAt: caller.AccessExpires.Format(time.RFC3339)}})
	response.Header().Set("Cache-Control", "no-store")
	return response, nil
}

func (s *browserService) Logout(ctx context.Context, req *connect.Request[api.LogoutRequest]) (*connect.Response[api.LogoutResponse], error) {
	if err := s.checkCSRF(req.Header()); err != nil {
		return nil, err
	}
	if refresh := cookieValue(req.Header(), refreshCookie); refresh != "" {
		if err := s.manager.RevokeRefresh(ctx, refresh); err != nil && !errors.Is(err, ErrUnauthenticated) {
			return nil, connect.NewError(connect.CodeInternal, errors.New("logout unavailable"))
		}
	}
	if access := cookieValue(req.Header(), accessCookie); access != "" {
		caller, err := s.manager.ValidateAccess(ctx, access)
		if err == nil {
			err = s.manager.Revoke(ctx, caller)
		}
		if err != nil && !errors.Is(err, ErrUnauthenticated) {
			return nil, connect.NewError(connect.CodeInternal, errors.New("logout unavailable"))
		}
	}
	response := connect.NewResponse(&api.LogoutResponse{})
	clearSessionCookies(response.Header())
	return response, nil
}

func (s *browserService) checkOrigin(header http.Header) error {
	if header.Get("Origin") != s.origin || (header.Get("Sec-Fetch-Site") != "" && header.Get("Sec-Fetch-Site") != "same-origin") {
		return connect.NewError(connect.CodePermissionDenied, errors.New("request origin denied"))
	}
	return nil
}

func (s *browserService) checkCSRF(header http.Header) error {
	if err := s.checkOrigin(header); err != nil {
		return err
	}
	provided, stored := header.Get("X-Blaxsmith-CSRF"), cookieValue(header, csrfCookie)
	if !validCSRFToken(provided) || subtle.ConstantTimeCompare([]byte(provided), []byte(stored)) != 1 {
		return connect.NewError(connect.CodePermissionDenied, errors.New("csrf token denied"))
	}
	return nil
}

func validCSRFToken(token string) bool {
	if len(token) != 43 {
		return false
	}
	bytes, err := base64.RawURLEncoding.DecodeString(token)
	return err == nil && len(bytes) == 32
}

func cookieValue(header http.Header, name string) string {
	request := &http.Request{Header: header}
	cookie, err := request.Cookie(name)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func browserCookie(name, value string, seconds int) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: seconds,
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode}
}

func setSessionCookies(header http.Header, tokens Tokens) {
	header.Add("Set-Cookie", browserCookie(accessCookie, tokens.Access, int(accessLifetime.Seconds())).String())
	header.Add("Set-Cookie", browserCookie(refreshCookie, tokens.Refresh, int(sessionLifetime.Seconds())).String())
	header.Set("Cache-Control", "no-store")
}

func clearSessionCookies(header http.Header) {
	header.Add("Set-Cookie", browserCookie(accessCookie, "", -1).String())
	header.Add("Set-Cookie", browserCookie(refreshCookie, "", -1).String())
	header.Set("Cache-Control", "no-store")
}

func tokenIdentity(tokens Tokens) *api.SessionIdentity {
	return &api.SessionIdentity{OrganizationId: tokens.Organization, PrincipalId: tokens.Principal,
		Role: tokens.Role, AccessExpiresAt: tokens.AccessExpires.Format(time.RFC3339)}
}

func peerAddress(addr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("parse peer address: %w", err)
	}
	return netip.ParseAddr(host)
}

func browserAuthError(err error) *connect.Error {
	switch {
	case errors.Is(err, ErrRateLimited):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many attempts; try again soon"))
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrRefreshReuse):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
	default:
		return connect.NewError(connect.CodeInternal, errors.New("authentication unavailable"))
	}
}
