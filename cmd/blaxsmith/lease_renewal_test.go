package main

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

type fakeGuestFile struct {
	data []byte
	mode uint32
}

type fakeGuest struct {
	files  map[string]fakeGuestFile
	writes []string
}

func (g *fakeGuest) ReadFile(_ context.Context, path string, max int) ([]byte, error) {
	f, ok := g.files[path]
	if !ok {
		return nil, errors.New("not found")
	}
	if len(f.data) > max {
		return nil, errors.New("too large")
	}
	return f.data, nil
}

func (g *fakeGuest) WriteFile(_ context.Context, path string, data []byte, mode uint32) error {
	g.files[path] = fakeGuestFile{append([]byte(nil), data...), mode}
	g.writes = append(g.writes, path)
	return nil
}

// The oauth_access hook writes the refreshed auth.json over the worker's
// recorded $CODEX_HOME/auth.json (0600) before announcing the new expiry,
// which never exceeds the token's expiry minus 5 minutes.
func TestOAuthRenewalHookRewritesCodexAuthInPlace(t *testing.T) {
	const authPath = "/tmp/blaxsmith-tool-123456/.codex/auth.json"
	tokenExpiry := time.Now().Add(40 * time.Minute).Truncate(time.Second)
	file := []byte(`{"OPENAI_API_KEY":null,"tokens":{"id_token":"id-2","access_token":"access-2","refresh_token":"","account_id":"a"},"last_refresh":"2026-09-24T00:00:00Z"}`)
	var renewed []access.RenewedLease
	delivery := func(file []byte, leaseExpiry time.Time) func(context.Context, access.RenewedLease) (access.Delivery, time.Time, error) {
		return func(_ context.Context, lease access.RenewedLease) (access.Delivery, time.Time, error) {
			renewed = append(renewed, lease)
			return access.Delivery{AccessToken: []byte("access-2"), ExpiresAt: tokenExpiry, FileName: ".codex/auth.json",
				File: append([]byte(nil), file...)}, leaseExpiry, nil
		}
	}
	lease := access.RenewedLease{OrganizationID: "org", AttemptID: "attempt", LeaseID: "lease", DeliveryMode: "oauth_access",
		ExpiresAt: time.Now().Add(30 * time.Minute)}
	newGuest := func(recorded string) *fakeGuest {
		return &fakeGuest{files: map[string]fakeGuestFile{
			codexAuthPathFile: {[]byte(recorded), 0o600},
			authPath:          {[]byte(`{"tokens":{"access_token":"access-1","refresh_token":""}}`), 0o600},
		}}
	}

	// Lease past the token's safe window: capped at exp-5m.
	guest := newGuest(authPath + "\n")
	renewers := map[string]deliveryRenewer{"oauth_access": codexRenewer(delivery(file, tokenExpiry))}
	if err := announceRenewal(t.Context(), renewers, lease, guest); err != nil {
		t.Fatal(err)
	}
	if got := guest.files[authPath]; string(got.data) != string(file) || got.mode != 0o600 {
		t.Fatalf("auth.json not rewritten in place: %+v", got)
	}
	if len(guest.writes) != 2 || guest.writes[0] != authPath || guest.writes[1] != leaseExpiresPath {
		t.Fatalf("auth.json must be written before the expiry notice: %q", guest.writes)
	}
	announced, err := strconv.ParseInt(strings.TrimSpace(string(guest.files[leaseExpiresPath].data)), 10, 64)
	if err != nil || announced != tokenExpiry.Add(-5*time.Minute).Unix() {
		t.Fatalf("announced %d, want token expiry minus 5m (%d): %v", announced, tokenExpiry.Add(-5*time.Minute).Unix(), err)
	}
	if len(renewed) != 1 || renewed[0] != lease {
		t.Fatalf("renewal got %+v", renewed)
	}

	// Within the window: the renewed lease expiry is announced as is.
	guest = newGuest(authPath + "\n")
	within := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	renewers["oauth_access"] = codexRenewer(delivery(file, within))
	if err := announceRenewal(t.Context(), renewers, lease, guest); err != nil ||
		string(guest.files[leaseExpiresPath].data) != strconv.FormatInt(within.Unix(), 10)+"\n" {
		t.Fatalf("announce within window: %q %v", guest.files[leaseExpiresPath].data, err)
	}

	// Fail closed: no write at all, and no expiry notice.
	for name, c := range map[string]struct {
		recorded string
		file     []byte
		mode     string
	}{
		"path outside CODEX_HOME":  {"/etc/passwd\n", file, "oauth_access"},
		"path traversal":           {"/tmp/blaxsmith-tool-1/../../etc/.codex/auth.json\n", file, "oauth_access"},
		"no recorded path":         {"", file, "oauth_access"},
		"refresh token in payload": {authPath + "\n", []byte(`{"tokens":{"access_token":"a","refresh_token":"rt"}}`), "oauth_access"},
		"unknown delivery mode":    {authPath + "\n", file, "short_lived"},
	} {
		guest := newGuest(c.recorded)
		if c.recorded == "" {
			delete(guest.files, codexAuthPathFile)
		}
		renewers := map[string]deliveryRenewer{"oauth_access": codexRenewer(delivery(c.file, within))}
		bad := lease
		bad.DeliveryMode = c.mode
		if err := announceRenewal(t.Context(), renewers, bad, guest); err == nil || len(guest.writes) != 0 {
			t.Fatalf("%s: renewal delivered: writes=%q err=%v", name, guest.writes, err)
		}
	}
	failing := codexRenewer(func(context.Context, access.RenewedLease) (access.Delivery, time.Time, error) {
		return access.Delivery{}, time.Time{}, access.ErrReconnect
	})
	guest = newGuest(authPath + "\n")
	if err := announceRenewal(t.Context(), map[string]deliveryRenewer{"oauth_access": failing}, lease, guest); !errors.Is(err, access.ErrReconnect) || len(guest.writes) != 0 {
		t.Fatalf("failed refresh was announced: %q %v", guest.writes, err)
	}

	// The registered map has the hook only when a refresher exists.
	if _, ok := deliveryRenewers(nil)["oauth_access"]; ok {
		t.Fatal("oauth_access renewal registered without a refresher")
	}
	if _, ok := deliveryRenewers(&access.OAuthRefresher{})["oauth_access"]; !ok {
		t.Fatal("oauth_access renewal hook is not registered")
	}
	guest = newGuest(authPath + "\n")
	if err := announceRenewal(t.Context(), deliveryRenewers(nil), access.RenewedLease{DeliveryMode: "native_raw", ExpiresAt: within}, guest); err != nil ||
		len(guest.writes) != 1 || guest.writes[0] != leaseExpiresPath {
		t.Fatalf("native_raw renewal: %q %v", guest.writes, err)
	}
}
