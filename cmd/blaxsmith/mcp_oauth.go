package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/identity"
)

type oauthClient struct {
	ID        string   `json:"client_id"`
	Name      string   `json:"client_name"`
	Redirects []string `json:"redirect_uris"`
}
type mcpOAuth struct {
	clients  map[string]oauthClient
	sessions *identity.SessionManager
	guard    *identity.BrowserGuard
	origin   string
}
type oauthRequest struct {
	client                     oauthClient
	redirect, state, challenge string
	scopes                     []string
}

var oauthClientID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
var oauthChallenge = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func newMCPOAuth(sessions *identity.SessionManager, guard *identity.BrowserGuard, origin string) (*mcpOAuth, error) {
	path := os.Getenv("BLAXSMITH_MCP_OAUTH_CLIENTS_FILE")
	if path == "" {
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open MCP OAuth client registry")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (32<<10)+1))
	if err != nil || len(data) > 32<<10 {
		return nil, errors.New("MCP OAuth client registry exceeds 32 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var clients []oauthClient
	if decoder.Decode(&clients) != nil || len(clients) == 0 || len(clients) > 32 {
		return nil, errors.New("invalid MCP OAuth client registry")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, errors.New("invalid MCP OAuth client registry")
	}
	o := &mcpOAuth{clients: map[string]oauthClient{}, sessions: sessions, guard: guard, origin: origin}
	for _, c := range clients {
		if !oauthClientID.MatchString(c.ID) || strings.TrimSpace(c.Name) != c.Name || c.Name == "" || len(c.Name) > 80 || strings.ContainsAny(c.Name, "\x00\r\n") || len(c.Redirects) == 0 || len(c.Redirects) > 8 || o.clients[c.ID].ID != "" {
			return nil, errors.New("invalid MCP OAuth client registration")
		}
		for _, redirect := range c.Redirects {
			if !validOAuthRedirect(redirect) {
				return nil, errors.New("OAuth redirects require HTTPS or a loopback IP callback without fragments or reserved response parameters")
			}
		}
		o.clients[c.ID] = c
	}
	return o, nil
}

func validOAuthRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.String() != raw {
		return false
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, key := range []string{"code", "state", "iss", "error", "error_description", "error_uri"} {
		if query.Has(key) {
			return false
		}
	}
	if u.Scheme == "https" {
		return true
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return u.Scheme == "http" && err == nil && ip.IsLoopback() && u.Port() != ""
}

func registeredRedirect(c oauthClient, raw string) bool {
	if !validOAuthRedirect(raw) {
		return false
	}
	u, _ := url.Parse(raw)
	for _, registered := range c.Redirects {
		if raw == registered {
			return true
		}
		// Native loopback callbacks choose an ephemeral port (RFC 8252).
		v, _ := url.Parse(registered)
		if u.Scheme == "http" && v.Scheme == "http" && u.Hostname() == v.Hostname() && u.EscapedPath() == v.EscapedPath() && u.RawQuery == v.RawQuery {
			return true
		}
	}
	return false
}

func (o *mcpOAuth) request(raw string) (oauthRequest, error) {
	var out oauthRequest
	if len(raw) > 8192 {
		return out, identity.ErrOAuthGrant
	}
	q, err := url.ParseQuery(raw)
	if err != nil {
		return out, identity.ErrOAuthGrant
	}
	for _, key := range []string{"response_type", "client_id", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method", "resource"} {
		if len(q[key]) > 1 {
			return out, identity.ErrOAuthGrant
		}
	}
	c, ok := o.clients[q.Get("client_id")]
	if !ok || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !oauthChallenge.MatchString(q.Get("code_challenge")) || q.Get("resource") != o.origin+"/mcp" || !registeredRedirect(c, q.Get("redirect_uri")) || len(q.Get("state")) > 1024 {
		return out, identity.ErrOAuthGrant
	}
	scopes := strings.Fields(q.Get("scope"))
	if len(scopes) == 0 {
		scopes = []string{"project.read"}
	}
	if len(scopes) > len(identity.APIScopes) || !slices.Contains(scopes, "project.read") {
		return out, identity.ErrOAuthGrant
	}
	slices.Sort(scopes)
	for i, scope := range scopes {
		if !slices.Contains(identity.APIScopes, scope) || i > 0 && scopes[i-1] == scope {
			return out, identity.ErrOAuthGrant
		}
	}
	return oauthRequest{c, q.Get("redirect_uri"), q.Get("state"), q.Get("code_challenge"), scopes}, nil
}

func oauthJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func oauthError(w http.ResponseWriter, status int, code string) {
	oauthJSON(w, status, map[string]string{"error": code})
}

func (o *mcpOAuth) callback(in oauthRequest, code string) string {
	u, _ := url.Parse(in.redirect)
	q := u.Query()
	q.Set("iss", o.origin)
	if in.state != "" {
		q.Set("state", in.state)
	}
	if code == "" {
		q.Set("error", "access_denied")
	} else {
		q.Set("code", code)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func (o *mcpOAuth) register(mux *http.ServeMux) {
	mux.Handle("GET /.well-known/oauth-protected-resource/mcp", o.guard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthJSON(w, 200, map[string]any{"resource": o.origin + "/mcp", "authorization_servers": []string{o.origin}, "scopes_supported": []string{"project.read"}, "bearer_methods_supported": []string{"header"}})
	})))
	mux.Handle("GET /.well-known/oauth-authorization-server", o.guard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		oauthJSON(w, 200, map[string]any{"issuer": o.origin, "authorization_endpoint": o.origin + "/oauth/authorize", "token_endpoint": o.origin + "/oauth/token", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": identity.APIScopes, "authorization_response_iss_parameter_supported": true})
	})))
	mux.Handle("GET /oauth/authorize", o.guard.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := o.request(r.URL.RawQuery); err != nil {
			oauthError(w, 400, "invalid_request")
			return
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		http.Redirect(w, r, "/oauth/consent?request="+url.QueryEscape(r.URL.RawQuery), http.StatusSeeOther)
	})))
	mux.Handle("POST /oauth/request", o.guard.Wrap(http.HandlerFunc(o.consent)))
	mux.Handle("POST /oauth/approve", o.guard.Wrap(http.HandlerFunc(o.consent)))
	mux.Handle("POST /oauth/token", o.guard.Wrap(http.HandlerFunc(o.token)))
}

func (o *mcpOAuth) consent(w http.ResponseWriter, r *http.Request) {
	approve := r.URL.Path == "/oauth/approve"
	caller, err := o.guard.Caller(r.Context(), r.Header, approve)
	if err != nil {
		oauthError(w, http.StatusForbidden, "access_denied")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	var body struct {
		Request   string   `json:"request"`
		ProjectID string   `json:"project_id"`
		Scopes    []string `json:"scopes"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(&body) != nil || decoder.Decode(&extra) != io.EOF {
		oauthError(w, 400, "invalid_request")
		return
	}
	in, err := o.request(body.Request)
	if err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	if !approve {
		oauthJSON(w, 200, map[string]any{"client_name": in.client.Name, "client_id": in.client.ID, "redirect_uri": in.redirect, "resource": o.origin + "/mcp", "scopes": in.scopes, "deny_url": o.callback(in, "")})
		return
	}
	if !slices.Contains(body.Scopes, "project.read") {
		oauthError(w, 400, "invalid_scope")
		return
	}
	for _, scope := range body.Scopes {
		if !slices.Contains(in.scopes, scope) {
			oauthError(w, 400, "invalid_scope")
			return
		}
	}
	code, err := o.sessions.AuthorizeOAuth(r.Context(), caller, identity.OAuthGrant{ClientID: in.client.ID, ClientName: in.client.Name, RedirectURI: in.redirect, Resource: o.origin + "/mcp", Challenge: in.challenge, ProjectID: body.ProjectID, Scopes: body.Scopes})
	if err != nil {
		oauthError(w, 400, "access_denied")
		return
	}
	oauthJSON(w, 200, map[string]string{"redirect_url": o.callback(in, code)})
}

func (o *mcpOAuth) token(w http.ResponseWriter, r *http.Request) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		oauthError(w, 400, "invalid_request")
		return
	}
	if err := o.sessions.AllowOAuthExchange(r.Context(), peer.Addr()); err != nil {
		if errors.Is(err, identity.ErrRateLimited) {
			w.Header().Set("Retry-After", "60")
			oauthError(w, 429, "temporarily_unavailable")
		} else {
			oauthError(w, 503, "server_error")
		}
		return
	}
	if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Origin") != "" {
		oauthError(w, 400, "invalid_request")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		oauthError(w, 400, "invalid_request")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if r.ParseForm() != nil || r.URL.RawQuery != "" {
		oauthError(w, 400, "invalid_request")
		return
	}
	for _, key := range []string{"grant_type", "client_id", "code", "code_verifier", "redirect_uri", "resource"} {
		if len(r.PostForm[key]) > 1 {
			oauthError(w, 400, "invalid_request")
			return
		}
	}
	if r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, 400, "unsupported_grant_type")
		return
	}
	if _, ok := o.clients[r.PostForm.Get("client_id")]; !ok {
		oauthError(w, 400, "invalid_client")
		return
	}
	token, raw, err := o.sessions.ExchangeOAuth(r.Context(), r.PostForm.Get("code"), r.PostForm.Get("client_id"), r.PostForm.Get("redirect_uri"), r.PostForm.Get("resource"), r.PostForm.Get("code_verifier"))
	if err != nil {
		status, code := http.StatusServiceUnavailable, "server_error"
		if errors.Is(err, identity.ErrOAuthGrant) || errors.Is(err, identity.ErrUnauthenticated) {
			status, code = 400, "invalid_grant"
		}
		oauthError(w, status, code)
		return
	}
	oauthJSON(w, 200, map[string]any{"access_token": raw, "token_type": "Bearer", "expires_in": 3600, "scope": strings.Join(token.Scopes, " ")})
}

func (o *mcpOAuth) challenge() string {
	return fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource/mcp", scope="project.read"`, o.origin)
}
