package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func connectionSecrets(t *testing.T, store *Store) *access.SecretStore {
	t.Helper()
	secrets, err := access.NewSecretStore(store.pool, "primary", map[string][]byte{"primary": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	return secrets
}

var testModels = []access.CatalogModel{{ID: "gpt-5", DisplayName: "GPT-5"}, {ID: "text-embedding-3", DisplayName: "Embeddings"}}

func TestConnectionScopeRules(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "conn-scope")
	owner := reviewer(t, pool, org, "owner", "conn-scope-owner")
	member := reviewer(t, pool, org, "member", "conn-scope-member")
	other := reviewer(t, pool, org, "member", "conn-scope-other")
	viewer := reviewer(t, pool, org, "viewer", "conn-scope-viewer")
	secrets := connectionSecrets(t, store)
	mine, err := store.CreateProjectAs(tenant.System(t.Context()), member, "conn-mine", "Mine")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := store.CreateProjectAs(tenant.System(t.Context()), other, "conn-theirs", "Theirs")
	if err != nil {
		t.Fatal(err)
	}
	// The UI's project-admin read matches the rule project connection writes enforce.
	for _, c := range []struct {
		name    string
		caller  identity.Caller
		project string
		want    bool
	}{{"owner", owner, theirs, true}, {"creator", member, mine, true}, {"other member", member, theirs, false}, {"viewer", viewer, mine, false}} {
		if got, err := store.CanAdministerProject(tenant.System(t.Context()), c.caller, c.project); err != nil || got != c.want {
			t.Fatalf("%s CanAdministerProject = %v, %v; want %v", c.name, got, err, c.want)
		}
	}
	if CanLaunch(viewer) || !CanLaunch(member) {
		t.Fatal("CanLaunch disagrees with LaunchRun's role rule")
	}
	key := []byte("sk-scope-secret-value")
	if _, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), member, ScopeOrganization, "", "openai", "", key, testModels, "", secrets); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member created an organization connection: %v", err)
	}
	if _, err := store.CreateGitTokenConnectionAs(tenant.System(t.Context()), member, ScopeOrganization, "", "github.com", "bot", "token", key, secrets); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member created an organization Git connection: %v", err)
	}
	if _, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), owner, ScopeOrganization, "", "openai", "Company", key, testModels, "", secrets); err != nil {
		t.Fatalf("owner organization connection: %v", err)
	}
	c, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), member, ScopeProject, mine, "opencode", "", key, testModels, "", secrets)
	if err != nil || c.Scope != ScopeProject || c.OwnerID != mine || c.ModelCount != 2 {
		t.Fatalf("project admin on own project: %+v %v", c, err)
	}
	if _, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), member, ScopeProject, theirs, "openai", "", key, testModels, "", secrets); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("project admin created a connection in another project: %v", err)
	}
	if _, err := store.CreateGitTokenConnectionAs(tenant.System(t.Context()), viewer, ScopeProject, mine, "github.com", "bot", "token", key, secrets); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("viewer created a project connection: %v", err)
	}
	if _, err := store.ListConnectionsAs(tenant.System(t.Context()), member, ScopeOrganization, ""); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member listed organization connections: %v", err)
	}
	if _, err := store.ListConnectionsAs(tenant.System(t.Context()), member, ScopeProject, theirs); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member listed another project's connections: %v", err)
	}
	personal, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), viewer, ScopePersonal, "", "anthropic", "", key, nil, "", secrets)
	if err != nil || personal.Scope != ScopePersonal || personal.OwnerID != viewer.PrincipalID {
		t.Fatalf("personal key: %+v %v", personal, err)
	}
	if err := store.RevokeConnectionAs(tenant.System(t.Context()), member, personal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("member revoked someone else's personal connection: %v", err)
	}
	if _, err := store.CreateCodexSubscriptionAs(tenant.System(t.Context()), member, []byte(`{"tokens":{}}`), secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid auth.json accepted: %v", err)
	}
	if _, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), owner, ScopeOrganization, "", "claude-subscription", "", key, nil, "", secrets); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown provider accepted: %v", err)
	}
}

func TestConnectionGrantVisibilityAndSecretsNeverReturned(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "conn-grant")
	owner := reviewer(t, pool, org, "owner", "conn-grant-owner")
	member := reviewer(t, pool, org, "member", "conn-grant-member")
	outsider := reviewer(t, pool, org, "member", "conn-grant-outsider")
	secrets := connectionSecrets(t, store)
	granted, err := store.CreateProjectAs(tenant.System(t.Context()), member, "conn-granted", "Granted")
	if err != nil {
		t.Fatal(err)
	}
	ungranted, err := store.CreateProjectAs(tenant.System(t.Context()), member, "conn-ungranted", "Ungranted")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "sk-live-never-return-this"
	c, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), owner, ScopeOrganization, "", "openai", "Company", []byte(secret), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	available := func(caller, project string) int {
		t.Helper()
		who := member
		if caller == "outsider" {
			who = outsider
		}
		items, err := store.ListConnectionsAs(tenant.System(t.Context()), who, ScopeProjectAvailable, project)
		if err != nil {
			t.Fatal(err)
		}
		return len(items)
	}
	if available("member", granted) != 0 {
		t.Fatal("ungranted organization connection was visible to a project")
	}
	if _, err := store.GrantConnectionAs(tenant.System(t.Context()), member, c.ID, granted, "project", ""); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member granted an organization connection: %v", err)
	}
	grant, err := store.GrantConnectionAs(tenant.System(t.Context()), owner, c.ID, granted, "project", "")
	if err != nil || grant.ProjectID != granted || grant.GranteeKind != "project" {
		t.Fatalf("grant: %+v %v", grant, err)
	}
	if available("member", granted) != 1 || available("member", ungranted) != 0 {
		t.Fatal("project grant visibility is wrong")
	}
	if _, err := store.AddConnectionUseAs(tenant.System(t.Context()), member, c.ID, ungranted, "gpt-5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted project used an organization connection: %v", err)
	}
	if _, err := store.AddConnectionUseAs(tenant.System(t.Context()), outsider, c.ID, granted, "gpt-5"); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("non-admin of the project attached a connection: %v", err)
	}
	use, err := store.AddConnectionUseAs(tenant.System(t.Context()), member, c.ID, granted, "gpt-5")
	if err != nil || use.GranteeKind != "workload" {
		t.Fatalf("use: %+v %v", use, err)
	}
	selection, err := store.ResolveModelGrant(tenant.System(t.Context()), org, granted, member.PrincipalID, "openai", "gpt-5")
	if err != nil || selection.GrantID != use.ID || selection.GranteeKind != "workload" || selection.SelectionID == "" {
		t.Fatalf("project run did not resolve the use: %+v %v", selection, err)
	}
	userGrant, err := store.GrantConnectionAs(tenant.System(t.Context()), owner, c.ID, "", "user", outsider.PrincipalID)
	if err != nil || userGrant.GranteeName != "conn-grant-outsider" {
		t.Fatalf("user grant: %+v %v", userGrant, err)
	}
	if available("outsider", ungranted) != 1 {
		t.Fatal("user grant did not make the connection available to that user")
	}
	orgList, err := store.ListConnectionsAs(tenant.System(t.Context()), owner, ScopeOrganization, "")
	if err != nil || len(orgList) != 1 || len(orgList[0].Grants) != 2 || len(orgList[0].Uses) != 1 {
		t.Fatalf("organization list: %+v %v", orgList, err)
	}
	models, _, _, err := store.ListConnectionModelsAs(tenant.System(t.Context()), member, c.ID, "codex")
	if err != nil || len(models) != 1 || models[0].ID != "gpt-5" {
		t.Fatalf("codex-filtered models: %+v %v", models, err)
	}
	if _, _, _, err := store.ListConnectionModelsAs(tenant.System(t.Context()), reviewer(t, pool, org, "viewer", "conn-grant-viewer"), c.ID, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer read an ungranted connection's models: %v", err)
	}
	for _, value := range []any{orgList, models, grant, use, c} {
		data, err := json.Marshal(value)
		if err != nil || strings.Contains(string(data), secret) {
			t.Fatalf("secret material reached a response: %s %v", data, err)
		}
	}
	var stored int
	if err := pool.QueryRow(tenant.System(t.Context()), `SELECT count(*) FROM access_connections c WHERE c.id=$1
		AND (c.external_account_id LIKE '%'||$2||'%' OR COALESCE(c.label,'') LIKE '%'||$2||'%')`, c.ID, secret).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("secret stored outside secret custody: %d %v", stored, err)
	}
	if err := store.RevokeConnectionGrantAs(tenant.System(t.Context()), owner, grant.ID); err != nil {
		t.Fatal(err)
	}
	if available("member", granted) != 0 {
		t.Fatal("revoked project grant stayed visible")
	}
	if _, err := store.ResolveModelGrant(tenant.System(t.Context()), org, granted, member.PrincipalID, "openai", "gpt-5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking the project grant left the project's use live: %v", err)
	}
}

func TestPersonalGrantHonouredOnlyForItsOwnersRuns(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "conn-personal")
	owner := reviewer(t, pool, org, "owner", "conn-personal-owner")
	alice := reviewer(t, pool, org, "member", "conn-personal-alice")
	bob := reviewer(t, pool, org, "member", "conn-personal-bob")
	secrets := connectionSecrets(t, store)
	project, err := store.CreateProjectAs(tenant.System(t.Context()), alice, "conn-personal", "Personal")
	if err != nil {
		t.Fatal(err)
	}
	company, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), owner, ScopeOrganization, "", "openai", "", []byte("sk-company"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantConnectionAs(tenant.System(t.Context()), owner, company.ID, project, "project", ""); err != nil {
		t.Fatal(err)
	}
	companyUse, err := store.AddConnectionUseAs(tenant.System(t.Context()), alice, company.ID, project, "gpt-5")
	if err != nil {
		t.Fatal(err)
	}
	personal, err := store.CreateAPIKeyConnectionAs(tenant.System(t.Context()), alice, ScopePersonal, "", "openai", "", []byte("sk-alice"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddConnectionUseAs(tenant.System(t.Context()), bob, personal.ID, project, "gpt-5"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob used alice's personal connection: %v", err)
	}
	personalUse, err := store.AddConnectionUseAs(tenant.System(t.Context()), alice, personal.ID, project, "gpt-5")
	if err != nil || personalUse.GranteeKind != "user" {
		t.Fatalf("personal use: %+v %v", personalUse, err)
	}
	resolve := func(initiator string) ProjectModelGrant {
		t.Helper()
		g, err := store.ResolveModelGrant(tenant.System(t.Context()), org, project, initiator, "openai", "gpt-5")
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if g := resolve(alice.PrincipalID); g.GrantID != personalUse.ID || g.GranteeKind != "user" || g.GranteeID != alice.PrincipalID || g.SelectionID != "" {
		t.Fatalf("alice's run did not use her own connection: %+v", g)
	}
	if g := resolve(bob.PrincipalID); g.GrantID != companyUse.ID || g.GranteeKind != "workload" {
		t.Fatalf("bob's run used alice's connection: %+v", g)
	}
	if g := resolve(""); g.GrantID != companyUse.ID {
		t.Fatalf("a run without an initiator used a personal grant: %+v", g)
	}
	// A grant naming alice on someone else's connection is not personal.
	if _, err := pool.Exec(tenant.System(t.Context()), `UPDATE access_connections SET owner_id=$3 WHERE organization_id=$1 AND id=$2`,
		org, personal.ID, bob.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if g := resolve(alice.PrincipalID); g.GrantID != companyUse.ID {
		t.Fatalf("a user grant on a connection alice does not own was honoured: %+v", g)
	}
	tx, err := pool.Begin(tenant.System(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(tenant.System(t.Context()))
	if _, err := access.PreflightModelInvoke(tenant.System(t.Context()), tx, access.ModelGrant{OrganizationID: org, ProjectID: project,
		GrantID: companyUse.ID, GranteeKind: "workload", GranteeID: access.DispatcherGrantee, Provider: "openai", Model: "gpt-5"}); err != nil {
		t.Fatalf("company use does not pass preflight: %v", err)
	}
}
