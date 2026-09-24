package gitfetch

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for raw, want := range map[string]bool{
		"140.82.114.3": true, "2606:50c0:8000::154": false,
		"127.0.0.1": false, "10.42.0.1": false, "100.64.0.1": false,
		"169.254.169.254": false, "192.0.2.1": false, "198.18.0.1": false,
		"::1": false, "fd00::1": false, "2001:db8::1": false, "64:ff9b::a2a:1": false,
	} {
		if got := PublicIPv4(netip.MustParseAddr(raw)); got != want {
			t.Errorf("PublicIPv4(%s) = %v, want %v", raw, got, want)
		}
	}
}

func TestFetchPublic(t *testing.T) {
	if os.Getenv("BLAXSMITH_TEST_PUBLIC_GIT") == "" {
		t.Skip("set BLAXSMITH_TEST_PUBLIC_GIT for a live public Git fetch")
	}
	source, err := Fetch(context.Background(), "https://github.com/octocat/Hello-World.git", "master")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	if len(source.Commit) != 40 {
		t.Fatalf("invalid commit %q", source.Commit)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(t.Context(), "https://github.com/octocat/Hello-World.git", "master", source.Commit, workspace); err != nil {
		t.Fatalf("exact public checkout: %v", err)
	}
}

func TestCheckoutRequiresEmptyWorkspace(t *testing.T) {
	for _, source := range []string{"file:///etc/passwd", "http://github.com/owner/repo", "https://127.0.0.1/owner/repo",
		"https://github.com/owner/repo?token=x", "https://github.com/owner/../repo"} {
		if err := Validate(source, "main"); err == nil {
			t.Fatalf("unsafe source accepted: %s", source)
		}
	}
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "existing"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Checkout(t.Context(), "https://github.com/owner/repo", "main", "0123456789012345678901234567890123456789", workspace); err == nil {
		t.Fatal("nonempty workspace accepted")
	}
}
