package gateway

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postGateway(t *testing.T, server *Server, path, body string) (int, string) {
	t.Helper()
	front := httptest.NewServer(server)
	defer front.Close()
	request, _ := http.NewRequest(http.MethodPost, front.URL+path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(out)
}

// TestBedrockCountTokens: count_tokens goes to Bedrock CountTokens with the
// InvokeModel body base64-wrapped and signed, and the answer comes back in
// Anthropic's shape.
func TestBedrockCountTokens(t *testing.T) {
	var path, auth string
	var invoke map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.EscapedPath(), r.Header.Get("Authorization")
		var wrapped struct {
			Input struct {
				InvokeModel struct {
					Body string `json:"body"`
				} `json:"invokeModel"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&wrapped)
		raw, _ := base64.StdEncoding.DecodeString(wrapped.Input.InvokeModel.Body)
		_ = json.Unmarshal(raw, &invoke)
		_, _ = io.WriteString(w, `{"inputTokens":1234}`)
	}))
	defer upstream.Close()
	credential, _ := json.Marshal(AWSCredential{AccessKeyID: "AKIAEXAMPLEEXAMPLE01", SecretAccessKey: "not-a-real-secret-key-for-tests-only"})
	route := Route{ID: "br", Kind: KindBedrock, AuthMethod: AWSSigV4AuthMethod, Region: "us-east-1",
		ModelMap: map[string]string{"claude-opus-5-5": "anthropic.claude-opus-5-5-v1:0"}, State: "enabled"}
	server := &Server{Upstream: map[string]string{KindBedrock: upstream.URL}, Record: (&recorder{}).record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan:     func(context.Context, Grant) (Plan, error) { return Plan{Routes: []Route{route}}, nil },
		RouteKey: func(context.Context, Grant, Route) ([]byte, error) { return append([]byte(nil), credential...), nil }}
	code, body := postGateway(t, server, "/anthropic/v1/messages/count_tokens", `{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 || strings.TrimSpace(body) != `{"input_tokens":1234}` {
		t.Fatalf("count: %d %s", code, body)
	}
	if path != "/model/anthropic.claude-opus-5-5-v1%3A0/count-tokens" || !strings.Contains(auth, "/us-east-1/bedrock/aws4_request") {
		t.Fatalf("request: %s %s", path, auth)
	}
	if invoke["anthropic_version"] != "bedrock-2023-05-31" || invoke["max_tokens"] != float64(1) || invoke["model"] != nil || invoke["messages"] == nil {
		t.Fatalf("wrapped invoke body: %v", invoke)
	}
}

// TestVertexCountTokens: count_tokens goes to the count-tokens publisher
// model with the route's model named in the body.
func TestVertexCountTokens(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pkcs8, _ := x509.MarshalPKCS8PrivateKey(key)
	account, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "gw@example-project.iam.gserviceaccount.com",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}))})
	tokens := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"access_token":"ya29.test","expires_in":3600}`)
	}))
	defer tokens.Close()
	var path string
	var sent map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&sent)
		_, _ = io.WriteString(w, `{"input_tokens":77}`)
	}))
	defer upstream.Close()
	route := Route{ID: "vx", Kind: KindVertex, AuthMethod: GCPServiceAccountAuthMethod, Region: "us-east5", CloudProject: "example-project",
		ModelMap: map[string]string{"claude-opus-5-5": "claude-opus-5-5@20260901"}, State: "enabled"}
	server := &Server{Upstream: map[string]string{KindVertex: upstream.URL}, VertexTokenURL: tokens.URL, Record: (&recorder{}).record,
		Authorize: func(context.Context, string, string) (Grant, error) {
			return testGrant("anthropic", "claude-opus-5-5"), nil
		},
		Plan:     func(context.Context, Grant) (Plan, error) { return Plan{Routes: []Route{route}}, nil },
		RouteKey: func(context.Context, Grant, Route) ([]byte, error) { return append([]byte(nil), account...), nil }}
	code, body := postGateway(t, server, "/anthropic/v1/messages/count_tokens", `{"model":"claude-opus-5-5","messages":[]}`)
	if code != 200 || !strings.Contains(body, `"input_tokens":77`) {
		t.Fatalf("count: %d %s", code, body)
	}
	if path != "/v1/projects/example-project/locations/us-east5/publishers/anthropic/models/count-tokens:rawPredict" ||
		sent["model"] != "claude-opus-5-5@20260901" || sent["anthropic_version"] != "vertex-2023-10-16" {
		t.Fatalf("vertex count request: %s %v", path, sent)
	}
}

// TestAzureOpenAIRoute: Chat Completions go to the model's deployment with
// the api-version and the api-key header; Responses go to /openai/responses
// with the deployment as the model. The gateway token never goes upstream.
func TestAzureOpenAIRoute(t *testing.T) {
	type seen struct{ path, query, apiKey, auth, model string }
	var got []seen
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got = append(got, seen{r.URL.Path, r.URL.RawQuery, r.Header.Get("Api-Key"), r.Header.Get("Authorization"), body.Model})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "99")
		w.Header().Set("X-Ratelimit-Limit-Requests", "100")
		_, _ = io.WriteString(w, `{"model":"gpt-6-luna","usage":{"prompt_tokens":20,"completion_tokens":5}}`)
	}))
	defer upstream.Close()
	route := Route{ID: "az", Kind: KindAzure, AuthMethod: AzureAPIKeyAuthMethod, AzureResource: "contoso-ai", APIVersion: "2024-10-21",
		ModelMap: map[string]string{"gpt-6-luna": "luna-prod"}, State: "enabled"}
	events := &recorder{}
	server := &Server{Upstream: map[string]string{KindAzure: upstream.URL}, Record: events.record,
		Authorize: func(context.Context, string, string) (Grant, error) { return testGrant("openai", "gpt-6-luna"), nil },
		Plan:      func(context.Context, Grant) (Plan, error) { return Plan{Routes: []Route{route}}, nil },
		RouteKey:  func(context.Context, Grant, Route) ([]byte, error) { return []byte("azure-test-key-0123456789"), nil }}
	for _, path := range []string{"/openai/v1/chat/completions", "/openai/v1/responses"} {
		if code, body := postGateway(t, server, path, `{"model":"gpt-6-luna","messages":[]}`); code != 200 {
			t.Fatalf("%s: %d %s", path, code, body)
		}
	}
	want := []seen{
		{"/openai/deployments/luna-prod/chat/completions", "api-version=2024-10-21", "azure-test-key-0123456789", "", "luna-prod"},
		{"/openai/responses", "api-version=2024-10-21", "azure-test-key-0123456789", "", "luna-prod"},
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("azure requests:\n got %+v\nwant %+v", got, want)
	}
	if e := events.last(t); e.RouteKind != KindAzure || e.Usage.Input != 20 {
		t.Fatalf("azure metering: %+v", e)
	}
	// An unmapped model is not served, and a resource is a name, never a URL.
	if _, ok := route.model("gpt-other"); ok {
		t.Fatal("unmapped model served on Azure")
	}
	if validAzureResource("evil.example.com/") || validAzureResource("") || !validAzureResource("contoso-ai") {
		t.Fatal("azure resource validation")
	}
}
