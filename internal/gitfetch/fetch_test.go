package gitfetch

import (
	"context"
	"net/netip"
	"os"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for raw, want := range map[string]bool{
		"140.82.114.3": true, "2606:50c0:8000::154": true,
		"127.0.0.1": false, "10.42.0.1": false, "100.64.0.1": false,
		"169.254.169.254": false, "192.0.2.1": false, "198.18.0.1": false,
		"::1": false, "fd00::1": false, "2001:db8::1": false,
	} {
		if got := publicIP(netip.MustParseAddr(raw)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", raw, got, want)
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
}
