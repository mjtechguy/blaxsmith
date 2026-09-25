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

// Every cookie is __Host- (Secure, Path=/, no Domain: a sibling subdomain
// cannot set or shadow it) and HttpOnly. The CSRF token reaches script through
// the GetCsrf response body, never document.cookie, so its cookie is HttpOnly
// too. The refresh cookie is SameSite=Strict: only same-site script calls
// RefreshSession and Logout, so no cross-site navigation ever needs it. The
// access cookie stays Lax because a top-level return from an OAuth provider
// (the GitHub callback) must still carry it. __Host- requires Path=/, which
// rules out scoping the refresh cookie to the AuthService path; the prefix's
// integrity guarantee is worth more than the narrower path.
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

// Caller requires a live membership at the time of each browser request, and
// refuses a session that must set an email first.
func (g *BrowserGuard) Caller(ctx context.Context, header http.Header, mutation bool) (Caller, error) {
	caller, err := g.AccountCaller(ctx, header, mutation)
	if err != nil {
		return Caller{}, err
	}
	return g.guarded(caller, nil)
}

// ErrEmailRequired refuses a one-time legacy session until it sets an email.
var ErrEmailRequired = errors.New("set your email to continue")

// guarded maps authentication errors and refuses email_required sessions.
func (g *BrowserGuard) guarded(caller Caller, err error) (Caller, error) {
	if err != nil {
		return Caller{}, browserAuthError(err)
	}
	if caller.EmailRequired {
		return Caller{}, connect.NewError(connect.CodeFailedPrecondition, ErrEmailRequired)
	}
	return caller, nil
}

// AccountCaller is Caller for the account-setup RPCs a session may use while
// it is still email_required. Everything else must use Caller.
func (g *BrowserGuard) AccountCaller(ctx context.Context, header http.Header, mutation bool) (Caller, error) {
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
	return g.guarded(g.manager.ValidateAccess(ctx, cookieValue(header, accessCookie)))
}

// RecheckStream revalidates a stream's caller against the live session. A
// stream authenticated once by StreamCaller survives its access token's
// expiry, but not revocation, expiry of the session, or a role change.
func (g *BrowserGuard) RecheckStream(ctx context.Context, caller Caller) (Caller, error) {
	return g.guarded(g.manager.CheckSession(ctx, caller))
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
	response.Header().Add("Set-Cookie", browserCookie(csrfCookie, token, 3600, http.SameSiteLaxMode).String())
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
	login := req.Msg.Email
	if login == "" {
		login = req.Msg.Username //nolint:staticcheck // Older clients send the login here.
	}
	tokens, err := s.manager.LoginLocal(ctx, req.Msg.OrganizationSlug, login, password, source, req.Header().Get("User-Agent"))
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
		Role: caller.Role, AccessExpiresAt: caller.AccessExpires.Format(time.RFC3339), EmailRequired: caller.EmailRequired}})
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

func browserCookie(name, value string, seconds int, site http.SameSite) *http.Cookie {
	return &http.Cookie{Name: name, Value: value, Path: "/", MaxAge: seconds,
		Secure: true, HttpOnly: true, SameSite: site}
}

// setSessionCookies keeps the refresh cookie exactly as long as the session:
// its lifetime slides with each refresh, up to the absolute limit.
func setSessionCookies(header http.Header, tokens Tokens) {
	header.Add("Set-Cookie", browserCookie(accessCookie, tokens.Access, int(accessLifetime.Seconds()), http.SameSiteLaxMode).String())
	refreshSeconds := max(int(time.Until(tokens.SessionExpires).Seconds()), 1)
	header.Add("Set-Cookie", browserCookie(refreshCookie, tokens.Refresh, refreshSeconds, http.SameSiteStrictMode).String())
	header.Set("Cache-Control", "no-store")
}

func clearSessionCookies(header http.Header) {
	header.Add("Set-Cookie", browserCookie(accessCookie, "", -1, http.SameSiteLaxMode).String())
	header.Add("Set-Cookie", browserCookie(refreshCookie, "", -1, http.SameSiteStrictMode).String())
	header.Set("Cache-Control", "no-store")
}

func tokenIdentity(tokens Tokens) *api.SessionIdentity {
	return &api.SessionIdentity{OrganizationId: tokens.Organization, PrincipalId: tokens.Principal,
		Role: tokens.Role, AccessExpiresAt: tokens.AccessExpires.Format(time.RFC3339), EmailRequired: tokens.EmailRequired}
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
	case errors.Is(err, ErrOrganizationRequired):
		return connect.NewError(connect.CodeFailedPrecondition, ErrOrganizationRequired)
	case errors.Is(err, ErrUnauthenticated), errors.Is(err, ErrRefreshReuse):
		return connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
	default:
		// A database outage or restart is transient: Unavailable tells the
		// browser to keep the session and retry rather than sign out.
		return connect.NewError(connect.CodeUnavailable, errors.New("authentication unavailable"))
	}
}
