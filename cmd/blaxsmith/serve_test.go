package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCatalogConnectContract(t *testing.T) {
	registry := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		name := "@opencode/cli"
		switch {
		case strings.Contains(request.URL.String(), "openai"):
			name = "@openai/codex"
		case strings.Contains(request.URL.String(), "anthropic"):
			name = "@anthropic-ai/claude-code"
		}
		body := fmt.Sprintf(`{"name":%q,"dist-tags":{"latest":"1.2.3"},"time":{"1.2.3":"2026-09-01T00:00:00Z"},"versions":{"1.2.3":{"dist":{"integrity":"sha512-test","tarball":%q}}}}`,
			name, "https://registry.npmjs.org/"+name+"/-/package-1.2.3.tgz")
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})}
	path, handler := apiv1connect.NewCatalogServiceHandler(catalogService{registry})
	mux := http.NewServeMux()
	mux.Handle("/api"+path, http.StripPrefix("/api", handler))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := apiv1connect.NewCatalogServiceClient(server.Client(), server.URL+"/api")
	response, err := client.ListTools(context.Background(), connect.NewRequest(&api.ListToolsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Msg.Tools) != 3 || response.Msg.Tools[0].Tool != "codex" ||
		response.Msg.Tools[2].Tool != "opencode" || response.Msg.Tools[0].Releases[0].Version != "1.2.3" {
		t.Fatalf("unexpected typed catalog: %+v", response.Msg.Tools)
	}
}
