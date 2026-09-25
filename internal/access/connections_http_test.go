package access

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestModelCatalogListsAndValidatesKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models" && r.Header.Get("Authorization") == "Bearer sk-openai":
			io.WriteString(w, `{"data":[{"id":"gpt-5","created":1700000000},{"id":"o4-mini","created":0}]}`)
		case r.URL.Path == "/v1/models" && r.Header.Get("x-api-key") == "sk-ant" && r.Header.Get("anthropic-version") == "2023-06-01":
			if r.URL.Query().Get("after_id") == "" {
				io.WriteString(w, `{"data":[{"id":"claude-opus-5","display_name":"Claude Opus 5","created_at":"2026-01-01T00:00:00Z","max_input_tokens":1000000,"capabilities":{"thinking":{"supported":true}}}],"has_more":true,"last_id":"claude-opus-5"}`)
			} else {
				io.WriteString(w, `{"data":[{"id":"claude-haiku-4-5","display_name":"Claude Haiku 4.5","created_at":"2025-10-01T00:00:00Z"}],"has_more":false}`)
			}
		case r.URL.Path == "/zen/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"gpt-5.1-codex","object":"model","created":1790266488}]}`)
		case r.URL.Path == "/zen/chat/completions":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"model":"gpt-5.1-codex"`) {
				w.WriteHeader(401)
				io.WriteString(w, `{"type":"error","error":{"type":"ModelError"}}`)
				return
			}
			if r.Header.Get("Authorization") != "Bearer zen-good" {
				w.WriteHeader(401)
				io.WriteString(w, `{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`)
				return
			}
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"InvalidRequest"}}`)
		default:
			w.WriteHeader(401)
		}
	}))
	defer srv.Close()
	c := ModelCatalog{Base: map[string]string{"openai": srv.URL, "anthropic": srv.URL, "opencode": srv.URL + "/zen"}}
	models, err := c.Fetch(tenant.System(t.Context()), "openai", "", []byte("sk-openai"))
	if err != nil || len(models) != 2 || models[0].ReleasedAt == nil || models[1].ReleasedAt != nil {
		t.Fatalf("openai: %+v %v", models, err)
	}
	if _, err := c.Fetch(tenant.System(t.Context()), "openai", "", []byte("bad")); !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("rejected openai key: %v", err)
	}
	models, err = c.Fetch(tenant.System(t.Context()), "anthropic", "", []byte("sk-ant"))
	if err != nil || len(models) != 2 || models[0].ContextTokens != 1000000 || len(models[0].Capabilities) == 0 ||
		models[0].DisplayName != "Claude Opus 5" {
		t.Fatalf("anthropic pages: %+v %v", models, err)
	}
	if models, err = c.Fetch(tenant.System(t.Context()), "opencode", "", []byte("zen-good")); err != nil || len(models) != 1 {
		t.Fatalf("opencode: %+v %v", models, err)
	}
	if _, err := c.Fetch(tenant.System(t.Context()), "opencode", "", []byte("zen-bad")); !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("rejected opencode key: %v", err)
	}
	if _, err := c.Fetch(tenant.System(t.Context()), "gemini", "", []byte("x")); !errors.Is(err, ErrDenied) {
		t.Fatalf("unknown provider: %v", err)
	}
}

func testJWT(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(claims)) + ".sig"
}

func TestCodexDeviceFlowExchangesOnceIntoCustodyShape(t *testing.T) {
	approved := false
	exchanges := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != codexClientID {
				w.WriteHeader(400)
				return
			}
			io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			if !approved {
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, `{"authorization_code":"code-1","code_challenge":"c","code_verifier":"v-1"}`)
		case "/oauth/token":
			exchanges++
			r.ParseForm()
			if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "code-1" ||
				r.Form.Get("code_verifier") != "v-1" || r.Form.Get("client_id") != codexClientID ||
				!strings.HasSuffix(r.Form.Get("redirect_uri"), "/deviceauth/callback") {
				w.WriteHeader(400)
				return
			}
			json.NewEncoder(w).Encode(map[string]string{
				"id_token":      testJWT(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct-1","chatgpt_plan_type":"plus"}}`),
				"access_token":  testJWT(`{"exp":` + jsonInt(time.Now().Add(time.Hour).Unix()) + `}`),
				"refresh_token": "rt-1"})
		}
	}))
	defer srv.Close()
	d := CodexDevice{Issuer: srv.URL}
	code, err := d.Start(tenant.System(t.Context()))
	if err != nil || code.UserCode != "ABCD-1234" || code.Interval != 5*time.Second || code.VerificationURL != srv.URL+"/codex/device" {
		t.Fatalf("start: %+v %v", code, err)
	}
	if _, err := d.Poll(tenant.System(t.Context()), code); !errors.Is(err, ErrDevicePending) {
		t.Fatalf("pending: %v", err)
	}
	approved = true
	bundle, err := d.Poll(tenant.System(t.Context()), code)
	if err != nil || exchanges != 1 {
		t.Fatalf("approved: %v exchanges=%d", err, exchanges)
	}
	_, account, err := ParseCodexAuth(bundle)
	if err != nil || account.AccountID != "acct-1" || account.PlanType != "plus" {
		t.Fatalf("device bundle must parse like a pasted auth.json: %+v %v", account, err)
	}
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestGitHubOAuthAndRepositoryDiscovery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			r.ParseForm()
			if r.Form.Get("code_verifier") != "verifier" || r.Form.Get("client_secret") != "shh" {
				io.WriteString(w, `{"error":"bad_verification_code"}`)
				return
			}
			io.WriteString(w, `{"access_token":"gho_token","token_type":"bearer"}`)
		case "/user":
			io.WriteString(w, `{"login":"octo"}`)
		case "/user/repos":
			if r.Header.Get("Authorization") != "Bearer gho_token" {
				w.WriteHeader(401)
				return
			}
			io.WriteString(w, `[{"full_name":"octo/app","clone_url":"https://github.com/octo/app.git","default_branch":"main","private":true},
				{"full_name":"octo/evil","clone_url":"https://evil.example/octo/evil.git","default_branch":"main"}]`)
		case "/repos/octo/app/branches":
			io.WriteString(w, `[{"name":"main"},{"name":"dev"}]`)
		}
	}))
	defer srv.Close()
	g := GitAPI{Base: map[string]string{"github-web": srv.URL, "github-api": srv.URL}}
	if u := g.GitHubAuthorizeURL("cid", "https://app/oauth/github/callback", "st", "ch"); !strings.Contains(u, "code_challenge_method=S256") || !strings.Contains(u, "state=st") {
		t.Fatalf("authorize url: %s", u)
	}
	if _, _, err := g.GitHubExchange(tenant.System(t.Context()), "cid", []byte("shh"), "code", "https://app/cb", "wrong"); err == nil {
		t.Fatal("wrong PKCE verifier accepted")
	}
	token, login, err := g.GitHubExchange(tenant.System(t.Context()), "cid", []byte("shh"), "code", "https://app/cb", "verifier")
	if err != nil || string(token) != "gho_token" || login != "octo" {
		t.Fatalf("exchange: %q %q %v", token, login, err)
	}
	repos, err := g.Repositories(tenant.System(t.Context()), "github.com", token, "APP")
	if err != nil || len(repos) != 1 || repos[0].FullName != "octo/app" || !repos[0].Private {
		t.Fatalf("repos (foreign clone URLs dropped): %+v %v", repos, err)
	}
	branches, err := g.Branches(tenant.System(t.Context()), "github.com", token, "octo/app")
	if err != nil || strings.Join(branches, ",") != "dev,main" {
		t.Fatalf("branches: %v %v", branches, err)
	}
	if _, err := g.Branches(tenant.System(t.Context()), "github.com", token, "../x"); !errors.Is(err, ErrDenied) {
		t.Fatalf("path traversal repo accepted: %v", err)
	}
}
