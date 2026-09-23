package catalog

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestFetchStableVersionsAndChannels(t *testing.T) {
	const source = "https://registry.npmjs.org/@anthropic-ai%2fclaude-code"
	metadata := `{"name":"@anthropic-ai/claude-code","dist-tags":{"latest":"2.1.10","stable":"2.1.9"},
		"time":{"2.1.9":"2026-09-19T01:00:00.000Z","2.1.10":"2026-09-20T01:00:00.000Z","2.1.11":"2026-09-21T01:00:00.000Z"},
		"versions":{
		"2.1.9":{"dist":{"integrity":"sha512-old","tarball":"https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.9.tgz"}},
		"2.1.10":{"dist":{"integrity":"sha512-new","tarball":"https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.10.tgz"}},
		"2.1.11":{"deprecated":"withdrawn","dist":{"integrity":"sha512-no","tarball":"https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.1.11.tgz"}},
		"2.2.0-beta.1":{"dist":{"integrity":"sha512-beta","tarball":"https://registry.npmjs.org/@anthropic-ai/claude-code/-/claude-code-2.2.0-beta.1.tgz"}}
		}}`
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != source || req.Header.Get("Accept") != "application/json" {
			t.Fatalf("unexpected catalog request: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(metadata)), Request: req}, nil
	})}
	result, err := Fetch(context.Background(), client, "claude-code", 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.LatestStable != "2.1.10" || result.PublisherLatest != "2.1.10" ||
		result.PublisherStable != "2.1.9" || len(result.Releases) != 1 ||
		result.Releases[0].Version != "2.1.10" || result.Releases[0].Integrity != "sha512-new" {
		t.Fatalf("wrong stable selection or publisher channel: %+v", result)
	}
	if _, err := Fetch(context.Background(), client, "unknown", 20); err == nil {
		t.Fatal("unapproved package accepted")
	}
}
