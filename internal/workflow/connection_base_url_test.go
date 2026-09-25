package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// Any API-key connection (organization, project, personal; any provider) may
// have a base URL; whoever manages the connection edits it, with audit.
func TestConnectionBaseURL(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "conn-base")
	owner := reviewer(t, pool, org, "owner", "conn-base-owner")
	member := reviewer(t, pool, org, "member", "conn-base-member")
	other := reviewer(t, pool, org, "member", "conn-base-other")
	secrets := connectionSecrets(t, store)
	const litellm = "https://litellm.example.com/v1"

	company, err := store.CreateAPIKeyConnectionAs(ctx, owner, ScopeOrganization, "", "openai", "Company", litellm,
		[]byte("sk-company"), testModels, "", secrets)
	if err != nil || company.BaseURL != litellm {
		t.Fatalf("organization key with a base URL: %+v %v", company, err)
	}
	if _, err := store.CreateAPIKeyConnectionAs(ctx, owner, ScopeOrganization, "", "openai", "", "https://litellm.example.com/",
		[]byte("sk-x"), nil, "", secrets); !errors.Is(err, access.ErrBaseURL) {
		t.Fatalf("an unnormalized base URL was stored: %v", err)
	}
	project, err := store.CreateProjectAs(ctx, member, "conn-base-project", "Base")
	if err != nil {
		t.Fatal(err)
	}
	local, err := store.CreateAPIKeyConnectionAs(ctx, member, ScopeProject, project, "anthropic", "", "https://gw.example.com",
		[]byte("sk-ant"), nil, access.ErrNoModelList.Error(), secrets)
	if err != nil || local.BaseURL != "https://gw.example.com" || local.ModelCount != 0 || local.ModelsError == "" {
		t.Fatalf("project key with a base URL that lists no models: %+v %v", local, err)
	}
	personal, err := store.CreateAPIKeyConnectionAs(ctx, member, ScopePersonal, "", "opencode", "", "", []byte("sk-oc"), testModels, "", secrets)
	if err != nil || personal.BaseURL != "" {
		t.Fatalf("personal key: %+v %v", personal, err)
	}

	// Managers only: org owners/admins, project admins, the personal owner.
	if err := store.SetConnectionBaseURLAs(ctx, member, company.ID, ""); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member edited an organization connection: %v", err)
	}
	if err := store.SetConnectionBaseURLAs(ctx, other, local.ID, ""); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("non-admin edited a project connection: %v", err)
	}
	if err := store.SetConnectionBaseURLAs(ctx, owner, personal.ID, litellm); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an owner edited someone's personal connection: %v", err)
	}
	if err := store.SetConnectionBaseURLAs(ctx, member, personal.ID, "http://10.0.0.1"); !errors.Is(err, access.ErrBaseURL) {
		t.Fatalf("invalid base URL accepted: %v", err)
	}
	if err := store.SetConnectionBaseURLAs(ctx, member, personal.ID, "https://oc.example.com/v1"); err != nil {
		t.Fatal(err)
	}
	updated, err := store.ManagedConnectionAs(ctx, member, personal.ID)
	if err != nil || updated.BaseURL != "https://oc.example.com/v1" || updated.ModelCount != 0 || updated.ModelsCheckedAt != nil {
		t.Fatalf("personal base URL change (old list dropped): %+v %v", updated, err)
	}
	if _, err := store.ManagedConnectionAs(ctx, owner, personal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an owner read someone's personal connection as manager: %v", err)
	}
	if err := store.SetConnectionBaseURLAs(ctx, owner, company.ID, ""); err != nil {
		t.Fatal(err)
	}
	var detail []byte
	if err := pool.QueryRow(ctx, `SELECT detail FROM identity_audit_events WHERE organization_id=$1
		AND action='access.connection.base_url_changed' AND subject_id=$2::uuid`, org, company.ID).Scan(&detail); err != nil {
		t.Fatalf("base URL change not audited: %v", err)
	}
	var change map[string]string
	if err := json.Unmarshal(detail, &change); err != nil || change["from"] != litellm || change["to"] != "" {
		t.Fatalf("audit detail: %s %v", detail, err)
	}
	// An unchanged value writes no audit event.
	if err := store.SetConnectionBaseURLAs(ctx, owner, company.ID, ""); err != nil {
		t.Fatal(err)
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND action='access.connection.base_url_changed'`, org).Scan(&audits); err != nil || audits != 2 {
		t.Fatalf("audit events: %d %v", audits, err)
	}
	listed, err := store.ListConnectionsAs(ctx, member, ScopeProject, project)
	if err != nil || len(listed) != 1 || listed[0].BaseURL != "https://gw.example.com" {
		t.Fatalf("list shows the base URL: %+v %v", listed, err)
	}
	secret, err := store.ReadConnectionSecretAs(ctx, member, local.ID, secrets)
	if err != nil || secret.BaseURL != "https://gw.example.com" {
		t.Fatalf("model refresh reads the base URL: %+v %v", secret.BaseURL, err)
	}
	secret.Secret.Clear()

	// Subscriptions never take a base URL.
	if _, err := pool.Exec(ctx, `UPDATE identity_organizations SET allow_member_claude_subscription=true WHERE id=$1`, org); err != nil {
		t.Fatal(err)
	}
	sub, err := store.CreateClaudeSubscriptionAs(ctx, member, []byte("sk-ant-oat01-"+strings.Repeat("a", 90)), secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetConnectionBaseURLAs(ctx, member, sub.ID, litellm); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a subscription took a base URL: %v", err)
	}
}
