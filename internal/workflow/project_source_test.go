package workflow

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestValidatePublicGitSource(t *testing.T) {
	lookup := func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host != "github.com" && host != "gitlab.com" {
			t.Fatalf("unexpected lookup: %s", host)
		}
		return []net.IPAddr{{IP: net.ParseIP("140.82.112.3")}}, nil
	}
	for _, tc := range []struct{ url, ref string }{
		{"https://github.com/owner/repo.git", "main"},
		{"https://gitlab.com/group/subgroup/repo", "release/v1.2"},
	} {
		url, ref, err := validatePublicGitSource(tenant.System(t.Context()), tc.url, tc.ref, lookup)
		if err != nil || url != tc.url || ref != tc.ref {
			t.Fatalf("valid source %q %q: %q %q %v", tc.url, tc.ref, url, ref, err)
		}
	}
	for _, tc := range []struct{ url, ref string }{
		{"ssh://github.com/owner/repo", ""},
		{"file:///etc/passwd", ""},
		{"https://localhost/owner/repo", ""},
		{"https://127.0.0.1/owner/repo", ""},
		{"https://github.com.evil.test/owner/repo", ""},
		{"https://user:token@github.com/owner/repo", ""},
		{"https://github.com:443/owner/repo", ""},
		{"https://github.com/owner/repo?token=x", ""},
		{"https://github.com/owner/repo#fragment", ""},
		{"https://github.com/owner/%2e%2e/repo", ""},
		{"https://github.com/owner/../repo", ""},
		{"https://github.com/owner/repo", "-config=value"},
		{"https://github.com/owner/repo", "a..b"},
		{"https://github.com/owner/repo", "feature/.lock"},
	} {
		if _, _, err := validatePublicGitSource(tenant.System(t.Context()), tc.url, tc.ref, lookup); !errors.Is(err, ErrInvalid) {
			t.Errorf("unsafe source %q %q: %v", tc.url, tc.ref, err)
		}
	}
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "198.18.0.1", "::1", "fc00::1", "2001:db8::1", "2002:a00:1::1"} {
		lookup := func(context.Context, string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP(address)}}, nil
		}
		if _, _, err := validatePublicGitSource(tenant.System(t.Context()), "https://github.com/owner/repo", "", lookup); !errors.Is(err, ErrSourceRoute) {
			t.Errorf("non-public route %s: %v", address, err)
		}
	}
	if _, _, err := validatePublicGitSource(tenant.System(t.Context()), "https://github.com/owner/repo", "", func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("140.82.112.3")}, {IP: net.ParseIP("10.1.2.3")}}, nil
	}); !errors.Is(err, ErrSourceRoute) {
		t.Errorf("mixed public/internal DNS answer: %v", err)
	}
	if _, _, err := validatePublicGitSource(tenant.System(t.Context()), "https://github.com/owner/repo", "", func(context.Context, string) ([]net.IPAddr, error) {
		return nil, errors.New("DNS unavailable")
	}); !errors.Is(err, ErrSourceRoute) {
		t.Errorf("DNS failure: %v", err)
	}
}
