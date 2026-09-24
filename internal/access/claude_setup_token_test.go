package access

import "testing"

func TestClaudeSetupTokenDeliveryIsOwnerOnly(t *testing.T) {
	cases := []struct {
		name                                           string
		mode, provider, ownerKind, ownerID, gKind, gID string
		want                                           bool
	}{
		{"owner's own run", "native_raw", "anthropic", "user", "alice", "user", "alice", true},
		{"another user", "native_raw", "anthropic", "user", "alice", "user", "bob", false},
		{"workload", "native_raw", "anthropic", "user", "alice", "workload", "dispatcher", false},
		{"role", "native_raw", "anthropic", "user", "alice", "role", "member", false},
		{"project-owned connection", "native_raw", "anthropic", "project", "p1", "user", "p1", false},
		{"org-owned connection", "native_raw", "anthropic", "organization", "o1", "user", "o1", false},
		{"wrong provider", "native_raw", "openai", "user", "alice", "user", "alice", false},
		{"empty owner", "native_raw", "anthropic", "user", "", "user", "", false},
		{"gateway mode", "brokered_gateway", "anthropic", "user", "alice", "user", "alice", false},
		{"oauth mode", "oauth_access", "anthropic", "user", "alice", "user", "alice", false},
	}
	for _, c := range cases {
		if got := deliveryAllowed(c.mode, ClaudeSetupTokenAuth, c.provider, c.ownerKind, c.ownerID, c.gKind, c.gID); got != c.want {
			t.Errorf("%s: deliveryAllowed = %v, want %v", c.name, got, c.want)
		}
	}
	// Unchanged: org API keys still deliver to workloads; Codex stays oauth_access owner-only.
	if !deliveryAllowed("native_raw", "api_key", "anthropic", "organization", "o1", "workload", "dispatcher") {
		t.Error("org API key no longer delivered natively")
	}
	if deliveryAllowed("oauth_access", CodexSubscriptionAuth, "openai", "user", "alice", "user", "bob") {
		t.Error("Codex subscription reached another user")
	}
}
