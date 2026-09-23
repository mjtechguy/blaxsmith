package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCatalogConnectContract(t *testing.T) {
	var calls atomic.Int32
	var unavailable atomic.Bool
	registry := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		time.Sleep(30 * time.Millisecond)
		if unavailable.Load() {
			return nil, errors.New("upstream unavailable")
		}
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
	service := &catalogService{client: registry}
	path, handler := apiv1connect.NewCatalogServiceHandler(service)
	mux := http.NewServeMux()
	mux.Handle("/api"+path, http.StripPrefix("/api", handler))
	server := httptest.NewServer(mux)
	defer server.Close()
	client := apiv1connect.NewCatalogServiceClient(server.Client(), server.URL+"/api")
	var wg sync.WaitGroup
	results := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response, err := client.ListTools(context.Background(), connect.NewRequest(&api.ListToolsRequest{}))
			if err != nil {
				results <- err
				return
			}
			if response.Msg.Stale || len(response.Msg.Tools) != 3 || response.Msg.Tools[0].Tool != "codex" ||
				response.Msg.Tools[2].Tool != "opencode" || response.Msg.Tools[0].Releases[0].Version != "1.2.3" {
				results <- fmt.Errorf("unexpected typed catalog: %+v", response.Msg.Tools)
			}
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("concurrent requests caused %d upstream fetches, want 3", calls.Load())
	}
	service.mu.Lock()
	service.freshUntil = time.Now().Add(-time.Minute)
	service.mu.Unlock()
	unavailable.Store(true)
	response, err := client.ListTools(context.Background(), connect.NewRequest(&api.ListToolsRequest{}))
	if err != nil || !response.Msg.Stale || calls.Load() != 4 {
		t.Fatalf("stale fallback failed: %v, %d upstream calls", err, calls.Load())
	}
	if _, err := client.ListTools(context.Background(), connect.NewRequest(&api.ListToolsRequest{})); err != nil || calls.Load() != 4 {
		t.Fatalf("backoff did not prevent another upstream call: %v, %d", err, calls.Load())
	}
	unavailable.Store(false)
	service.mu.Lock()
	service.retryAfter = time.Time{}
	service.mu.Unlock()
	response, err = client.ListTools(context.Background(), connect.NewRequest(&api.ListToolsRequest{}))
	if err != nil || response.Msg.Stale || calls.Load() != 7 {
		t.Fatalf("catalog did not recover: %v, %d upstream calls", err, calls.Load())
	}
	service.mu.Lock()
	service.freshUntil = time.Now().Add(-time.Minute)
	service.staleUntil = time.Now().Add(-time.Minute)
	service.retryAfter = time.Now().Add(time.Minute)
	service.mu.Unlock()
	if _, err := client.ListTools(context.Background(), connect.NewRequest(&api.ListToolsRequest{})); connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("expired catalog was served: %v", err)
	}
}
