package identity

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestBrowserSessionPostgres(t *testing.T) {
	pool := identityTestPool(t)
	ctx := tenant.System(context.Background())
	password := []byte("correct horse battery staple")
	owner, err := BootstrapOwner(ctx, pool, "alice@example.com", "engineering", "Engineering", password)
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewSessionManager(pool, "blaxsmith-test", signer)
	if err != nil {
		t.Fatal(err)
	}
	var handler http.Handler
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	origin := server.URL
	path, browserHandler, err := NewBrowserHandler(manager, origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewBrowserHandler(manager, "http://localhost"); err == nil {
		t.Fatal("plaintext origin accepted")
	}
	mux := http.NewServeMux()
	mux.Handle("/api"+path, http.StripPrefix("/api", browserHandler))
	handler = mux
	plaintext := httptest.NewRecorder()
	mux.ServeHTTP(plaintext, httptest.NewRequest(http.MethodPost, "http://"+strings.TrimPrefix(origin, "https://")+"/api"+path+"GetCsrf", nil))
	if plaintext.Code != http.StatusForbidden {
		t.Fatalf("plaintext browser auth accepted: %d", plaintext.Code)
	}
	wrongHost := httptest.NewRecorder()
	mux.ServeHTTP(wrongHost, httptest.NewRequest(http.MethodPost, "https://wrong.example/api"+path+"GetCsrf", nil))
	if wrongHost.Code != http.StatusForbidden {
		t.Fatalf("wrong host accepted: %d", wrongHost.Code)
	}
	client := server.Client()
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	apiClient := apiv1connect.NewAuthServiceClient(client, server.URL+"/api")
	getCsrf := connect.NewRequest(&api.GetCsrfRequest{})
	if _, err := apiClient.GetCsrf(ctx, getCsrf); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("missing origin accepted: %v", err)
	}
	getCsrf.Header().Set("Origin", origin)
	csrf, err := apiClient.GetCsrf(ctx, getCsrf)
	if err != nil || !validCSRFToken(csrf.Msg.Token) {
		t.Fatalf("csrf bootstrap failed: %v", err)
	}
	assertSecureCookies(t, csrf.Header(), csrfCookie)
	login := connect.NewRequest(&api.LoginLocalRequest{OrganizationSlug: "engineering", Email: "alice@example.com", Password: string(password)})
	login.Header().Set("Origin", origin)
	login.Header().Set("Sec-Fetch-Site", "cross-site")
	login.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	if _, err := apiClient.LoginLocal(ctx, login); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-site login accepted: %v", err)
	}
	login.Header().Del("Sec-Fetch-Site")
	login.Header().Del("X-Blaxsmith-CSRF")
	if _, err := apiClient.LoginLocal(ctx, login); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("missing csrf accepted: %v", err)
	}
	oversized := connect.NewRequest(&api.LoginLocalRequest{OrganizationSlug: "engineering", Email: "alice@example.com", Password: strings.Repeat("x", 5000)})
	oversized.Header().Set("Origin", origin)
	oversized.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	if _, err := apiClient.LoginLocal(ctx, oversized); connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("oversized login accepted: %v", err)
	}
	login.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	response, err := apiClient.LoginLocal(ctx, login)
	if err != nil || response.Msg.Session.OrganizationId != owner.OrganizationID || response.Msg.Session.Role != "owner" {
		t.Fatalf("local login failed: %+v, %v", response, err)
	}
	assertSecureCookies(t, response.Header(), accessCookie, refreshCookie)
	serverURL, _ := url.Parse(server.URL)
	refreshBefore := jarCookie(client.Jar.Cookies(serverURL), refreshCookie)
	if refreshBefore == "" {
		t.Fatal("refresh cookie missing")
	}
	current := connect.NewRequest(&api.CurrentSessionRequest{})
	current.Header().Set("Origin", origin)
	me, err := apiClient.CurrentSession(ctx, current)
	if err != nil || me.Msg.Session.PrincipalId != owner.PrincipalID {
		t.Fatalf("current session failed: %+v, %v", me, err)
	}
	refresh := connect.NewRequest(&api.RefreshSessionRequest{})
	refresh.Header().Set("Origin", origin)
	refresh.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='viewer'
		WHERE organization_id=$1 AND principal_id=$2`, owner.OrganizationID, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := apiClient.CurrentSession(ctx, current); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stale role accepted: %v", err)
	}
	rotated, err := apiClient.RefreshSession(ctx, refresh)
	if err != nil || rotated.Msg.Session.Role != "viewer" || jarCookie(client.Jar.Cookies(serverURL), refreshCookie) == refreshBefore {
		t.Fatalf("refresh did not rotate or update role: %+v, %v", rotated, err)
	}
	noJar := apiv1connect.NewAuthServiceClient(server.Client(), server.URL+"/api")
	expireGrace(t, pool)
	replay := connect.NewRequest(&api.RefreshSessionRequest{})
	replay.Header().Set("Origin", origin)
	replay.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	replay.Header().Set("Cookie", csrfCookie+"="+csrf.Msg.Token+"; "+refreshCookie+"="+refreshBefore)
	if _, err := noJar.RefreshSession(ctx, replay); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("replayed refresh accepted: %v", err)
	} else {
		var connectErr *connect.Error
		if !errors.As(err, &connectErr) {
			t.Fatal(err)
		}
		assertSecureCookies(t, connectErr.Meta(), accessCookie, refreshCookie)
	}
	if _, err := apiClient.CurrentSession(ctx, current); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("replayed refresh left access valid: %v", err)
	}
	if _, err := apiClient.LoginLocal(ctx, login); err != nil {
		t.Fatal(err)
	}
	fallback := connect.NewRequest(&api.LogoutRequest{})
	fallback.Header().Set("Origin", origin)
	fallback.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	fallback.Header().Set("Cookie", csrfCookie+"="+csrf.Msg.Token+"; "+refreshCookie+"="+jarCookie(client.Jar.Cookies(serverURL), refreshCookie))
	if _, err := noJar.Logout(ctx, fallback); err != nil {
		t.Fatalf("refresh-only logout failed: %v", err)
	}
	if _, err := apiClient.CurrentSession(ctx, current); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("refresh-only logout left access valid: %v", err)
	}
	if _, err := apiClient.LoginLocal(ctx, login); err != nil {
		t.Fatal(err)
	}
	logout := connect.NewRequest(&api.LogoutRequest{})
	logout.Header().Set("Origin", origin)
	logout.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	if _, err := apiClient.Logout(ctx, logout); err != nil {
		t.Fatal(err)
	}
	if _, err := apiClient.CurrentSession(ctx, current); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("logout left access valid: %v", err)
	}
	if jarCookie(client.Jar.Cookies(serverURL), refreshCookie) != "" {
		t.Fatal("logout left refresh cookie")
	}
}

func assertSecureCookies(t *testing.T, header http.Header, names ...string) {
	t.Helper()
	for _, name := range names {
		found := false
		for _, value := range header.Values("Set-Cookie") {
			if strings.HasPrefix(value, name+"=") {
				site := "SameSite=Lax"
				if name == refreshCookie {
					site = "SameSite=Strict"
				}
				found = strings.Contains(value, "Secure") && strings.Contains(value, "HttpOnly") &&
					strings.Contains(value, site) && strings.Contains(value, "Path=/") && !strings.Contains(value, "Domain=")
			}
		}
		if !found {
			t.Fatalf("%s missing secure cookie attributes: %v", name, header.Values("Set-Cookie"))
		}
	}
}

func jarCookie(cookies []*http.Cookie, name string) string {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie.Value
		}
	}
	return ""
}
