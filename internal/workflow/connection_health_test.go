package workflow

import (
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestDeriveConnectionHealth(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	checked := now.Add(-time.Hour)
	old := now.Add(-30 * 24 * time.Hour)
	latest := map[string]string{"claude-code": "2.1.10", "codex": "0.156.1"}
	pinned := map[string]string{"claude-code": "2.1.9", "codex": "0.156.1"}
	cases := []struct {
		name                  string
		c                     Connection
		state, auth, reason   string
		identity, checkedWant string
	}{
		{"ready key", Connection{Kind: "api_key", Provider: "openai", Label: "Team", State: "active", ModelsCheckedAt: &checked, PinnedVersions: pinned},
			"ready", "authenticated", "", "Team", checked.Format(time.RFC3339)},
		{"rejected key", Connection{Kind: "api_key", Provider: "openai", State: "active", ModelsCheckedAt: &checked, ModelsError: "the provider rejected this API key"},
			"error", "unauthenticated", "key_rejected", "", checked.Format(time.RFC3339)},
		{"provider down", Connection{Kind: "api_key", Provider: "openai", State: "active", ModelsCheckedAt: &checked, ModelsError: "provider returned HTTP 503"},
			"warning", "unknown", "provider_error", "", checked.Format(time.RFC3339)},
		{"unchecked key", Connection{Kind: "api_key", Provider: "opencode", State: "active"},
			"warning", "unknown", "not_checked", "", ""},
		{"needs sign-in", Connection{Kind: "subscription", Provider: "codex", Account: "acct-1", State: "reconnect_required", ReconnectReason: "refresh_rejected", RefreshedAt: &checked},
			"error", "unauthenticated", "needs_sign_in", "acct-1", checked.Format(time.RFC3339)},
		{"stale subscription", Connection{Kind: "subscription", Provider: "codex", Account: "acct-1", State: "active", CreatedAt: old},
			"warning", "authenticated", "token_expiring", "acct-1", ""},
		{"fresh subscription", Connection{Kind: "subscription", Provider: "codex", Account: "acct-1", State: "active", CreatedAt: old, RefreshedAt: &checked, PinnedVersions: pinned},
			"ready", "authenticated", "", "acct-1", checked.Format(time.RFC3339)},
		{"harness behind", Connection{Kind: "api_key", Provider: "anthropic", State: "active", ModelsCheckedAt: &checked, PinnedVersions: pinned},
			"warning", "authenticated", "harness_behind", "", checked.Format(time.RFC3339)},
		{"revoked", Connection{Kind: "api_key", Provider: "anthropic", State: "revoked", ModelsCheckedAt: &checked, PinnedVersions: pinned},
			"disabled", "unknown", "revoked", "", checked.Format(time.RFC3339)},
		{"git", Connection{Kind: "git", Provider: "github", Account: "octocat", State: "active"},
			"ready", "unknown", "", "octocat", ""},
	}
	for _, tc := range cases {
		h := DeriveHealth(tc.c, latest, now)
		got := ""
		if h.CheckedAt != nil {
			got = h.CheckedAt.Format(time.RFC3339)
		}
		if h.State != tc.state || h.Auth != tc.auth || h.Reason != tc.reason || h.Identity != tc.identity || got != tc.checkedWant {
			t.Errorf("%s: got %+v (checked %q)", tc.name, h, got)
		}
		if tc.reason != "" && h.Message == "" {
			t.Errorf("%s: no message", tc.name)
		}
	}
	h := DeriveHealth(Connection{Kind: "api_key", Provider: "anthropic", State: "active", ModelsCheckedAt: &checked, PinnedVersions: pinned}, latest, now)
	if h.Harness != "claude-code" || h.PinnedVersion != "2.1.9" || h.LatestVersion != "2.1.10" {
		t.Fatalf("version advisory: %+v", h)
	}
	if versionLess("0.156.1", "0.156.1") || !versionLess("0.9.0", "0.10.0") || versionLess("x", "1.0.0") {
		t.Fatal("versionLess")
	}
}

// The store reads refresh state and the newest approved runtime per harness.
func TestConnectionHealthInputsFromStore(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "health")
	owner := reviewer(t, pool, org, "owner", "health-owner")
	secrets := connectionSecrets(t, store)
	for _, version := range []string{"2.1.9", "2.1.10", "2.0.99"} {
		if _, err := pool.Exec(tenant.System(t.Context()), `INSERT INTO workflow_tool_runtime_approvals
			(organization_id,harness,model,effort,image,binary_path,binary_sha256,version,worker_pool,max_timeout_seconds,max_output_bytes,approved_by)
			VALUES ($1,'claude-code','claude-opus-5-'||$2,'high','img@sha256:'||repeat('a',64),'/bin/claude',repeat('b',64),$2,'pool',60,1024,'test')`,
			org, version); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), owner, ScopeOrganization, "", "anthropic", "Prod", []byte("sk-health"), nil,
		"the provider rejected this API key", secrets); err != nil {
		t.Fatal(err)
	}
	list, err := store.ListConnectionsAs(tenant.System(t.Context()), owner, ScopeOrganization, "")
	if err != nil || len(list) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if list[0].PinnedVersions["claude-code"] != "2.1.10" {
		t.Fatalf("pinned %v", list[0].PinnedVersions)
	}
	if h := DeriveHealth(list[0], nil, time.Now()); h.Reason != "key_rejected" || h.Identity != "Prod" || h.CheckedAt == nil {
		t.Fatalf("health %+v", h)
	}
}
