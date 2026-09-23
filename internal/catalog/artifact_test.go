package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

type artifactTransport func(*http.Request) (*http.Response, error)

func (f artifactTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestDownloadVerifiedLive(t *testing.T) {
	if os.Getenv("BLAXSMITH_LIVE_CATALOG") != "1" {
		t.Skip("set BLAXSMITH_LIVE_CATALOG=1 for an official npm release probe")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := &http.Client{Timeout: 2 * time.Minute}
	for _, tool := range []string{"codex", "claude-code", "opencode"} {
		result, err := Fetch(ctx, client, tool, 1)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := DownloadVerified(ctx, client, tool, result.Releases[0], t.TempDir())
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		t.Logf("verified %s %s: %d bytes, sha256 %s", tool, result.LatestStable, artifact.Size, artifact.SHA256)
	}
}

func TestDownloadVerified(t *testing.T) {
	body := []byte("synthetic npm tarball")
	integrity := sha512.Sum512(body)
	wantSHA := sha256.Sum256(body)
	release := Release{Version: "1.2.3", Integrity: "sha512-" + base64.StdEncoding.EncodeToString(integrity[:]),
		Tarball: "https://registry.npmjs.org/@openai/codex/-/codex-1.2.3.tgz"}
	directory := t.TempDir()
	client := &http.Client{Transport: artifactTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)), Request: request}, nil
	})}
	artifact, err := DownloadVerified(context.Background(), client, "codex", release, directory)
	if err != nil || artifact.Size != int64(len(body)) || artifact.SHA256 != hex.EncodeToString(wantSHA[:]) {
		t.Fatalf("verified artifact: %+v, %v", artifact, err)
	}
	read, err := os.ReadFile(artifact.Path)
	if err != nil || !bytes.Equal(read, body) {
		t.Fatalf("artifact bytes: %q, %v", read, err)
	}
	info, err := os.Stat(artifact.Path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact permissions: %v, %v", info, err)
	}
	if err := os.Remove(artifact.Path); err != nil {
		t.Fatal(err)
	}
	bad := release
	bad.Integrity = "sha512-" + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0}, sha512.Size))
	if _, err := DownloadVerified(context.Background(), client, "codex", bad, directory); err == nil {
		t.Fatal("tampered integrity accepted")
	}
	bad = release
	bad.Tarball = "https://example.test/codex-1.2.3.tgz"
	if _, err := DownloadVerified(context.Background(), client, "codex", bad, directory); err == nil {
		t.Fatal("untrusted tarball host accepted")
	}
	bad = release
	bad.Version = "1.2.3-beta"
	if _, err := DownloadVerified(context.Background(), client, "codex", bad, directory); err == nil {
		t.Fatal("unapproved prerelease accepted")
	}
	oversized := &http.Client{Transport: artifactTransport(func(request *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")),
			ContentLength: maxTarballBytes + 1, Request: request}, nil
	})}
	if _, err := DownloadVerified(context.Background(), oversized, "codex", release, directory); err == nil {
		t.Fatal("oversized release accepted")
	}
	redirected := &http.Client{Transport: artifactTransport(func(request *http.Request) (*http.Response, error) {
		other, _ := url.Parse("https://elsewhere.test/payload")
		request.URL = other
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)),
			ContentLength: int64(len(body)), Request: request}, nil
	})}
	if _, err := DownloadVerified(context.Background(), redirected, "codex", release, directory); err == nil {
		t.Fatal("redirected release accepted")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed release left files: %v, %v", entries, err)
	}
}
