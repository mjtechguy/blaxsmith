package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Part of the real TLS/PostgreSQL suite; the browser suite covers the consent UI.
func testMCPOAuth(t *testing.T, ctx context.Context, pool *pgxpool.Pool, browser *http.Client, origin, csrf, project string) {
	t.Helper()
	plain := &http.Client{Transport: browser.Transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		response, err := plain.Get(origin + path)
		if err != nil {
			t.Fatal(err)
		}
		var metadata map[string]any
		err = json.NewDecoder(response.Body).Decode(&metadata)
		response.Body.Close()
		if err != nil || response.StatusCode != 200 {
			t.Fatalf("OAuth discovery: %d %v", response.StatusCode, err)
		}
		if path == "/.well-known/oauth-protected-resource/mcp" && metadata["resource"] != origin+"/mcp" {
			t.Fatal("wrong OAuth resource")
		}
		if path == "/.well-known/oauth-authorization-server" && metadata["issuer"] != origin {
			t.Fatal("wrong OAuth issuer")
		}
	}
	verifier := strings.Repeat("v", 43)
	proof := sha256.Sum256([]byte(verifier))
	query := url.Values{"response_type": {"code"}, "client_id": {"test-mcp"}, "redirect_uri": {"https://client.example.test/callback"}, "resource": {origin + "/mcp"}, "scope": {"project.read goal.write"}, "state": {"e2e-state"}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(proof[:])}}
	for _, change := range []struct{ key, value string }{{"redirect_uri", "https://untrusted.invalid/callback"}, {"code_challenge_method", "plain"}, {"resource", origin + "/machine"}, {"client_id", "unknown"}, {"scope", "admin"}} {
		invalid, _ := url.ParseQuery(query.Encode())
		invalid.Set(change.key, change.value)
		response, err := plain.Get(origin + "/oauth/authorize?" + invalid.Encode())
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 400 || response.Header.Get("Location") != "" {
			t.Fatalf("invalid OAuth %s redirected: %d", change.key, response.StatusCode)
		}
	}
	post := func(path string, body any, withCSRF bool) (int, map[string]any) {
		t.Helper()
		data, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(ctx, "POST", origin+path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", origin)
		if withCSRF {
			req.Header.Set("X-Blaxsmith-CSRF", csrf)
		}
		response, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var out map[string]any
		if json.NewDecoder(response.Body).Decode(&out) != nil {
			t.Fatal("invalid OAuth JSON response")
		}
		return response.StatusCode, out
	}
	body := map[string]any{"request": query.Encode(), "project_id": project, "scopes": []string{"project.read"}}
	if status, _ := post("/oauth/approve", body, false); status != 403 {
		t.Fatal("OAuth consent bypassed CSRF")
	}
	body["scopes"] = []string{"project.read", "run.launch"}
	if status, _ := post("/oauth/approve", body, true); status != 400 {
		t.Fatal("OAuth scope expansion accepted")
	}
	body["scopes"] = []string{"project.read"}
	authorize := func() string {
		t.Helper()
		status, out := post("/oauth/approve", body, true)
		if status != 200 {
			t.Fatalf("OAuth approval: %d %v", status, out)
		}
		callback, err := url.Parse(out["redirect_url"].(string))
		if err != nil || callback.Host != "client.example.test" || callback.Query().Get("iss") != origin || callback.Query().Get("state") != "e2e-state" {
			t.Fatal("OAuth callback lost client/issuer/state binding")
		}
		return callback.Query().Get("code")
	}
	code := authorize()
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {"test-mcp"}, "code": {code}, "resource": {origin + "/mcp"}, "code_verifier": {verifier}, "redirect_uri": {"https://client.example.test/callback"}}
	exchange := func(form url.Values) (int, map[string]any) {
		t.Helper()
		response, err := plain.PostForm(origin+"/oauth/token", form)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		var out map[string]any
		if json.NewDecoder(response.Body).Decode(&out) != nil {
			t.Fatal("invalid token response")
		}
		return response.StatusCode, out
	}
	for _, change := range []struct{ key, value string }{{"code_verifier", strings.Repeat("x", 43)}, {"resource", origin + "/machine"}, {"client_id", "other-mcp"}, {"redirect_uri", "https://other.example.test/callback"}} {
		invalid, _ := url.ParseQuery(form.Encode())
		invalid.Set(change.key, change.value)
		if status, _ := exchange(invalid); status != 400 {
			t.Fatalf("OAuth exchange accepted wrong %s", change.key)
		}
	}
	status, token := exchange(form)
	if status != 200 || token["scope"] != "project.read" || token["refresh_token"] != nil {
		t.Fatalf("OAuth exchange: %d", status)
	}
	raw := token["access_token"].(string)
	remoteHTTP := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.Header.Set("Authorization", "Bearer "+raw)
		return browser.Transport.RoundTrip(copy)
	}), Timeout: 5 * time.Second}
	remote, err := mcp.NewClient(&mcp.Implementation{Name: "oauth-e2e", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: origin + "/mcp", HTTPClient: remoteHTTP, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("OAuth MCP initialize: %v", err)
	}
	defer remote.Close()
	tools, err := remote.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) == 0 {
		t.Fatalf("OAuth MCP tools: %v", err)
	}
	for _, tool := range tools.Tools {
		if tool.Name == "blaxsmith_CreateGoal" || tool.Name == "blaxsmith_LaunchRun" {
			t.Fatal("OAuth tools exceeded consent")
		}
	}
	result, err := remote.CallTool(ctx, &mcp.CallToolParams{Name: "blaxsmith_GetProject", Arguments: map[string]any{"projectId": project}})
	if err != nil || result.IsError {
		t.Fatalf("OAuth project read: %v", err)
	}
	request, _ := http.NewRequestWithContext(ctx, "POST", origin+"/machine/blaxsmith.api.v1.WorkflowService/DescribeMachineAccess", strings.NewReader(`{}`))
	request.Header.Set("Authorization", "Bearer "+raw)
	request.Header.Set("Content-Type", "application/json")
	response, err := plain.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatal("MCP OAuth token crossed its resource boundary")
	}
	if status, _ := exchange(form); status != 400 {
		t.Fatal("authorization code was reusable")
	}
	if _, err := remote.ListTools(ctx, nil); err == nil {
		t.Fatal("code replay did not revoke its issued token")
	}
	// A new browser-approved grant is revocable through existing Agent access.
	form.Set("code", authorize())
	status, token = exchange(form)
	if status != 200 {
		t.Fatal("replacement consent failed")
	}
	raw = token["access_token"].(string)
	remote, err = mcp.NewClient(&mcp.Implementation{Name: "oauth-replacement-e2e", Version: "1"}, nil).Connect(ctx, &mcp.StreamableClientTransport{Endpoint: origin + "/mcp", HTTPClient: remoteHTTP, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatalf("replacement OAuth MCP initialize: %v", err)
	}
	defer remote.Close()
	if _, err := remote.ListTools(ctx, nil); err != nil {
		t.Fatalf("replacement OAuth token inactive before revocation: %v", err)
	}
	web := apiv1connect.NewWorkflowServiceClient(browser, origin+"/api")
	list := connect.NewRequest(&api.ListApiTokensRequest{ProjectId: project})
	list.Header().Set("Origin", origin)
	inventory, err := web.ListApiTokens(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	var live string
	for _, item := range inventory.Msg.Credentials {
		if item.Label == "MCP: E2E MCP" && item.RevokedAt == "" {
			live = item.Id
		}
	}
	if live == "" {
		t.Fatal("OAuth credential missing from Agent access")
	}
	revoke := connect.NewRequest(&api.RevokeApiTokenRequest{TokenId: live})
	revoke.Header().Set("Origin", origin)
	revoke.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := web.RevokeApiToken(ctx, revoke); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.ListTools(ctx, nil); err == nil {
		t.Fatal("revoked OAuth token accepted")
	}
	// Expired codes and database rate limits fail at the actual token endpoint.
	expired := authorize()
	hash := sha256.Sum256([]byte(expired))
	if _, err := pool.Exec(ctx, `UPDATE identity_oauth_codes SET expires_at=clock_timestamp()-interval '1 second' WHERE code_hash=$1`, hash[:]); err != nil {
		t.Fatal(err)
	}
	form.Set("code", expired)
	if status, _ := exchange(form); status != 400 {
		t.Fatal("expired code accepted")
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_login_limits SET attempts=60 WHERE scope='oauth_source'`); err != nil {
		t.Fatal(err)
	}
	if status, _ := exchange(form); status != 429 {
		t.Fatal("OAuth rate limit ignored")
	}
	if _, err := pool.Exec(ctx, `DELETE FROM identity_login_limits WHERE scope='oauth_source'`); err != nil {
		t.Fatal(err)
	}
	request, _ = http.NewRequestWithContext(ctx, "POST", origin+"/mcp", strings.NewReader(`{}`))
	response, err = plain.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode != 401 || !strings.Contains(response.Header.Get("WWW-Authenticate"), "oauth-protected-resource/mcp") {
		t.Fatal("missing OAuth discovery challenge")
	}
}
