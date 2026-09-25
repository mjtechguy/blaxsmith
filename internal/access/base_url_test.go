package access

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeBaseURL(t *testing.T) {
	for raw, want := range map[string]string{
		"":                                 "",
		"  ":                               "",
		"https://LiteLLM.Example.com":      "https://litellm.example.com",
		"https://litellm.example.com/":     "https://litellm.example.com",
		"https://litellm.example.com/v1//": "",
		"https://litellm.example.com/v1/":  "https://litellm.example.com/v1",
		" https://gw.example.com:8443/openai/v1 ": "https://gw.example.com:8443/openai/v1",
	} {
		got, err := NormalizeBaseURL(raw)
		if want == "" && raw != "" && strings.TrimSpace(raw) != "" {
			if !errors.Is(err, ErrBaseURL) {
				t.Errorf("%q: accepted as %q", raw, got)
			}
			continue
		}
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", raw, got, err, want)
		}
	}
	for _, bad := range []string{
		"http://litellm.example.com", "http://127.0.0.1:4000", "ftp://x.example", "litellm.example.com",
		"https://user:pass@x.example", "https://x.example?key=1", "https://x.example/?", "https://x.example#frag",
		"https://", "https:///v1", "https://x.example/a b", `https://x.example/"`, "https://x.example/%2e%2e",
		"https://x.example/v1/../admin", "https://x.example:99999", "https://x.example:port",
		"https://" + strings.Repeat("a", 510) + ".example",
	} {
		if got, err := NormalizeBaseURL(bad); !errors.Is(err, ErrBaseURL) {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
	// A local test server is allowed only through the test-only path.
	if got, err := normalizeBaseURL("http://127.0.0.1:4000/v1/", true); err != nil || got != "http://127.0.0.1:4000/v1" {
		t.Fatalf("loopback in tests: %q %v", got, err)
	}
	if _, err := normalizeBaseURL("http://litellm.example.com", true); !errors.Is(err, ErrBaseURL) {
		t.Fatal("http to a non-loopback host accepted")
	}
}

// The model list for a base URL comes from that endpoint, with the path each
// provider family uses; a list that is missing is not a key failure.
func TestModelCatalogFetchesFromBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("Authorization") == "Bearer bad" || r.Header.Get("x-api-key") == "bad":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/proxy/v1/models" && r.Header.Get("Authorization") == "Bearer sk-proxy":
			io.WriteString(w, `{"data":[{"id":"gpt-6-luna","created":1790000000},{"id":"claude-sonnet-5"}]}`)
		case r.URL.Path == "/anthropic/v1/models" && r.Header.Get("x-api-key") == "sk-proxy":
			io.WriteString(w, `{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}],"has_more":false}`)
		case r.URL.Path == "/redirect/models":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	base, err := normalizeBaseURL(srv.URL, true)
	if err != nil {
		t.Fatal(err)
	}
	client := endpointClient()
	client.Transport = srv.Client().Transport // the loopback server, with the endpoint client's no-redirect rule
	c := ModelCatalog{Endpoint: client}
	ctx := t.Context()
	models, err := c.Fetch(ctx, "openai", base+"/proxy/v1", []byte("sk-proxy"))
	if err != nil || len(models) != 2 || models[0].ID != "gpt-6-luna" {
		t.Fatalf("OpenAI-compatible list: %+v %v", models, err)
	}
	models, err = c.Fetch(ctx, "anthropic", base+"/anthropic", []byte("sk-proxy"))
	if err != nil || len(models) != 1 || models[0].DisplayName != "Claude Sonnet 5" {
		t.Fatalf("Anthropic-compatible list: %+v %v", models, err)
	}
	// OpenCode keys against a base URL are checked by the list alone: no
	// completion request is sent to an arbitrary endpoint.
	if models, err = c.Fetch(ctx, "opencode", base+"/proxy/v1", []byte("sk-proxy")); err != nil || len(models) != 2 {
		t.Fatalf("OpenCode through a base URL: %+v %v", models, err)
	}
	if _, err := c.Fetch(ctx, "openai", base+"/proxy/v1", []byte("bad")); !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("rejected key: %v", err)
	}
	if _, err := c.Fetch(ctx, "openai", base+"/nolist", []byte("sk-proxy")); !errors.Is(err, ErrNoModelList) {
		t.Fatalf("endpoint without a model list: %v", err)
	}
	if _, err := c.Fetch(ctx, "openai", base+"/redirect", []byte("sk-proxy")); !errors.Is(err, ErrNoModelList) {
		t.Fatalf("a redirect must not be followed: %v", err)
	}
}

// The endpoint client never connects to a loopback, private, or link-local
// address, whatever the URL names.
func TestEndpointClientRefusesInternalAddresses(t *testing.T) {
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer srv.Close()
	c := ModelCatalog{} // the real endpoint client
	for _, base := range []string{srv.URL, "http://localhost" + srv.URL[strings.LastIndex(srv.URL, ":"):],
		"https://169.254.169.254", "https://10.0.0.1", "https://[::1]"} {
		if _, err := c.Fetch(t.Context(), "openai", base, []byte("sk")); !errors.Is(err, ErrNoModelList) ||
			!strings.Contains(err.Error(), errEndpointAddress.Error()) {
			t.Errorf("%s: %v", base, err)
		}
	}
	if hit {
		t.Fatal("the endpoint client reached a loopback server")
	}
}
