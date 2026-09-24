package workflow

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ConnectionHealth is the one-line status every connection card shows. State
// is ready, warning, error, or disabled; Auth is authenticated,
// unauthenticated, or unknown. Reason is a stable code the UI maps to a
// one-click fix: key_rejected, needs_sign_in, token_expiring, provider_error,
// not_checked, harness_behind, revoked, or empty.
type ConnectionHealth struct {
	State, Auth, Identity, Reason, Message string
	CheckedAt                              *time.Time
	Harness, PinnedVersion, LatestVersion  string
}

// staleSubscription is how long a subscription may go without a token
// refresh before the card warns that its sign-in may lapse.
//
// ponytail: providers do not publish refresh-token lifetimes; three weeks idle
// is a conservative prompt, not a known expiry.
const staleSubscription = 21 * 24 * time.Hour

// connectionHarness is the pinned harness a connection mainly serves.
func connectionHarness(c Connection) string {
	switch {
	case c.Kind == "subscription":
		return "codex"
	case c.Kind != "api_key":
		return ""
	case c.Provider == "anthropic":
		return "claude-code"
	case c.Provider == "openai":
		return "codex"
	}
	return "opencode"
}

// DeriveHealth turns stored connection state into its health line. latest
// maps a harness to the tools catalog's latest stable version, when known.
func DeriveHealth(c Connection, latest map[string]string, now time.Time) ConnectionHealth {
	h := ConnectionHealth{State: "ready", Auth: "authenticated", Identity: c.Account}
	if c.Kind == "api_key" {
		h.Identity = c.Label
	}
	switch c.Kind {
	case "api_key":
		h.CheckedAt = c.ModelsCheckedAt
	case "subscription":
		h.CheckedAt = c.RefreshedAt
		if h.CheckedAt == nil {
			h.CheckedAt = c.ModelsCheckedAt
		}
	default:
		h.Auth = "unknown" // Git tokens are checked when used, not listed.
	}
	switch {
	case c.State == "revoked" || c.State == "disabled":
		h.State, h.Auth, h.Reason, h.Message = "disabled", "unknown", "revoked", "This connection is "+c.State+"; runs cannot use it."
	case c.State == "reconnect_required":
		h.State, h.Auth, h.Reason = "error", "unauthenticated", "needs_sign_in"
		h.Message = "The saved sign-in stopped working; sign in again."
		if c.ReconnectReason == "refresh_outcome_unknown" {
			h.Message = "A token refresh was interrupted and may have spent the sign-in; sign in again."
		}
	case c.Kind == "api_key" && c.ModelsError != "":
		h.State, h.Auth, h.Reason, h.Message = "warning", "unknown", "provider_error", c.ModelsError
		if strings.Contains(strings.ToLower(c.ModelsError), "rejected") {
			h.State, h.Auth, h.Reason, h.Message = "error", "unauthenticated", "key_rejected", "The provider rejected this API key; replace it."
		}
	case c.Kind == "api_key" && c.ModelsCheckedAt == nil:
		h.State, h.Auth, h.Reason, h.Message = "warning", "unknown", "not_checked", "The key has not been checked yet."
	case c.Kind == "subscription":
		last := c.CreatedAt
		if h.CheckedAt != nil {
			last = *h.CheckedAt
		}
		if idle := now.Sub(last); idle > staleSubscription {
			h.State, h.Reason = "warning", "token_expiring"
			h.Message = fmt.Sprintf("Not refreshed in %d days; the sign-in may expire. Sign in again if runs fail.", int(idle.Hours()/24))
		}
	}
	h.Harness = connectionHarness(c)
	h.PinnedVersion, h.LatestVersion = c.PinnedVersions[h.Harness], latest[h.Harness]
	if h.State == "ready" && h.PinnedVersion != "" && versionLess(h.PinnedVersion, h.LatestVersion) {
		h.State, h.Reason = "warning", "harness_behind"
		h.Message = fmt.Sprintf("Pinned %s %s is behind the catalog's latest %s.", h.Harness, h.PinnedVersion, h.LatestVersion)
	}
	return h
}

// versionLess compares dotted numeric versions; anything unparsable is not less.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	if len(pa) != 3 || len(pb) != 3 {
		return false
	}
	for i := range 3 {
		x, errX := strconv.Atoi(pa[i])
		y, errY := strconv.Atoi(pb[i])
		if errX != nil || errY != nil {
			return false
		}
		if x != y {
			return x < y
		}
	}
	return false
}
