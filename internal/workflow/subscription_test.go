package workflow

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestSubscriptionConnectionIsPersonalAndSecretFree(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "subscription")
	project, err := store.CreateProject(tenant.System(t.Context()), org, "subscription", "Subscription")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateProject(tenant.System(t.Context()), org, "subscription-other", "Subscription other")
	if err != nil {
		t.Fatal(err)
	}
	owner := reviewer(t, pool, org, "owner", "sub-owner")
	viewer := reviewer(t, pool, org, "viewer", "sub-viewer")
	secrets, err := access.NewSecretStore(pool, "primary", map[string][]byte{"primary": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	jwt := func(claims any) string {
		body, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
	}
	credential, _ := json.Marshal(map[string]any{"OPENAI_API_KEY": nil, "tokens": map[string]string{
		"access_token":  jwt(map[string]any{"exp": time.Now().Add(time.Hour).Unix()}),
		"refresh_token": "private-refresh-token",
		"id_token":      jwt(map[string]any{"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": "acct-1"}})}})

	if _, err := store.CreateSubscriptionConnectionAs(tenant.System(t.Context()), owner, project, "anthropic", "claude-test", credential, secrets); !errors.Is(err, access.ErrClaudeSubscriptionDisabled) {
		t.Fatalf("Claude subscription accepted: %v", err)
	}
	if _, err := store.CreateSubscriptionConnectionAs(tenant.System(t.Context()), owner, project, "openai", "gpt-5", []byte(`{"OPENAI_API_KEY":"sk"}`), secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed login accepted: %v", err)
	}
	if _, err := store.CreateSubscriptionConnectionAs(tenant.System(t.Context()), viewer, other, "openai", "gpt-5", credential, secrets); !errors.Is(err, ErrSubscriptionPolicy) {
		t.Fatalf("member bypassed project policy: %v", err)
	}
	item, err := store.CreateSubscriptionConnectionAs(tenant.System(t.Context()), owner, project, "openai", "gpt-5", credential, secrets)
	if err != nil || item.AccountID != "acct-1" || item.State != "active" {
		t.Fatalf("create: %+v %v", item, err)
	}
	var granteeKind, granteeID, mode string
	if err := pool.QueryRow(tenant.System(t.Context()), `SELECT grantee_kind,grantee_id,delivery_mode FROM access_grants
		WHERE organization_id=$1 AND connection_id=$2`, org, item.ConnectionID).Scan(&granteeKind, &granteeID, &mode); err != nil ||
		granteeKind != "user" || granteeID != owner.PrincipalID || mode != "oauth_access" {
		t.Fatalf("grant: %s %s %s %v", granteeKind, granteeID, mode, err)
	}
	listed, err := store.ListSubscriptionConnections(tenant.System(t.Context()), owner, project)
	if err != nil || len(listed) != 1 || listed[0].ConnectionID != item.ConnectionID {
		t.Fatalf("list: %+v %v", listed, err)
	}
	if body, _ := json.Marshal(listed); strings.Contains(string(body), "private-refresh-token") {
		t.Fatal("list leaked secret material")
	}
	if others, err := store.ListSubscriptionConnections(tenant.System(t.Context()), viewer, project); err != nil || len(others) != 0 {
		t.Fatalf("another member saw a personal login: %+v %v", others, err)
	}
	if err := store.RevokeSubscriptionConnectionAs(tenant.System(t.Context()), viewer, item.ConnectionID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner revoked: %v", err)
	}
	if err := store.RevokeSubscriptionConnectionAs(tenant.System(t.Context()), owner, item.ConnectionID); err != nil {
		t.Fatal(err)
	}
	listed, err = store.ListSubscriptionConnections(tenant.System(t.Context()), owner, project)
	if err != nil || len(listed) != 1 || listed[0].State != "revoked" {
		t.Fatalf("revoked list: %+v %v", listed, err)
	}
}
