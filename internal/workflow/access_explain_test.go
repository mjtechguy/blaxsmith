package workflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// ExplainAccessAs must agree with access.CanUse for every principal, project,
// and resource, and name the grant MatchingGrant returns.
func TestExplainAccessMatchesCanUse(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "explain")
	owner := reviewer(t, pool, org, "owner", "explain-owner")
	admin := reviewer(t, pool, org, "admin", "explain-admin")
	member := reviewer(t, pool, org, "member", "explain-member")
	named := reviewer(t, pool, org, "member", "explain-named")
	viewer := reviewer(t, pool, org, "viewer", "explain-viewer")
	secrets := connectionSecrets(t, store)
	granted, err := store.CreateProjectAs(ctx, member, "explain-granted", "Granted")
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateProjectAs(ctx, member, "explain-other", "Other")
	if err != nil {
		t.Fatal(err)
	}
	projectGrant, err := store.CreateAPIKeyConnectionAs(ctx, owner, ScopeOrganization, "", "anthropic", "Anthropic prod", "", []byte("sk-a"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	roleGrant, err := store.CreateAPIKeyConnectionAs(ctx, owner, ScopeOrganization, "", "openai", "", "", []byte("sk-b"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	ungranted, err := store.CreateAPIKeyConnectionAs(ctx, owner, ScopeOrganization, "", "openai", "Spare", "", []byte("sk-c"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	personal, err := store.CreateAPIKeyConnectionAs(ctx, member, ScopePersonal, "", "openai", "Mine", "", []byte("sk-d"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	projectOwned, err := store.CreateAPIKeyConnectionAs(ctx, member, ScopeProject, granted, "openai", "Local", "", []byte("sk-e"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	grant := func(c string, project, kind, grantee string) {
		t.Helper()
		if _, err := store.GrantConnectionAs(ctx, owner, c, project, kind, grantee); err != nil {
			t.Fatal(err)
		}
	}
	grant(projectGrant.ID, granted, "project", "")
	grant(projectGrant.ID, "", "user", named.PrincipalID)
	grant(roleGrant.ID, "", "role", "admin")
	grant(roleGrant.ID, "", "role", "member")
	orgRecipe, _, err := store.CreateRecipeAs(ctx, owner, "", "Explained", "", guildRecipe(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GrantRecipeAs(ctx, owner, orgRecipe.ID, "", "role", "admin"); err != nil {
		t.Fatal(err)
	}

	type resource struct{ kind, id string }
	orgResources := []resource{{access.ResourceConnection, projectGrant.ID}, {access.ResourceConnection, roleGrant.ID},
		{access.ResourceConnection, ungranted.ID}, {access.ResourceRecipe, orgRecipe.ID}}
	for _, principal := range []identity.Caller{owner, admin, member, named, viewer} {
		for _, project := range []string{granted, other} {
			for _, r := range orgResources {
				// Ask as the owner about each principal, as enforcement would.
				got, err := store.ExplainAccessAs(ctx, owner, project, r.kind, r.id, principal.PrincipalID)
				if err != nil {
					t.Fatal(err)
				}
				tx, err := pool.Begin(ctx)
				if err != nil {
					t.Fatal(err)
				}
				want, err := access.CanUse(ctx, tx, principal, project, r.kind, r.id)
				match, _, _ := access.MatchingGrant(ctx, tx, principal, project, r.kind, r.id)
				tx.Rollback(ctx)
				if err != nil {
					t.Fatal(err)
				}
				label := fmt.Sprintf("%s/%s in %s", principal.Role, r.id[:8], project[:8])
				if got.Usable != want || got.GrantID != match.GrantID {
					t.Errorf("%s: explain %+v, CanUse %v grant %q", label, got, want, match.GrantID)
				}
				if !got.Usable && got.Reason == "" {
					t.Errorf("%s: no reason", label)
				}
			}
		}
	}
	// The most specific grant wins: a project grant over a user grant.
	got, err := store.ExplainAccessAs(ctx, owner, granted, access.ResourceConnection, projectGrant.ID, named.PrincipalID)
	if err != nil || len(got.Steps) != 2 || got.Steps[0].Kind != "project_grant" || got.Steps[0].Label != "Granted" ||
		got.Steps[1].Kind != "organization_connection" || got.Steps[1].Label != "Anthropic prod" {
		t.Fatalf("project grant chain %+v %v", got, err)
	}
	if got, _ := store.ExplainAccessAs(ctx, owner, other, access.ResourceConnection, projectGrant.ID, named.PrincipalID); got.Steps[0].Kind != "user_grant" {
		t.Fatalf("user grant chain %+v", got)
	}
	if got, _ := store.ExplainAccessAs(ctx, owner, other, access.ResourceConnection, roleGrant.ID, admin.PrincipalID); got.Steps[0].Label != "admins and owners" {
		t.Fatalf("narrowest role grant %+v", got)
	}
	if got, _ := store.ExplainAccessAs(ctx, owner, other, access.ResourceRecipe, orgRecipe.ID, admin.PrincipalID); len(got.Steps) != 3 || got.Steps[2].Label != "v1 (current)" {
		t.Fatalf("recipe version source %+v", got)
	}
	// Personal and project connections are explained without grants.
	if got, _ := store.ExplainAccessAs(ctx, member, granted, access.ResourceConnection, personal.ID, ""); !got.Usable || got.Steps[0].Kind != "personal_connection" {
		t.Fatalf("own personal connection %+v", got)
	}
	if got, _ := store.ExplainAccessAs(ctx, owner, granted, access.ResourceConnection, personal.ID, named.PrincipalID); got.Usable {
		t.Fatalf("someone else's personal connection %+v", got)
	}
	if got, _ := store.ExplainAccessAs(ctx, owner, other, access.ResourceConnection, projectOwned.ID, ""); got.Usable {
		t.Fatalf("another project's connection %+v", got)
	}
	// Members may only ask about themselves, and learn nothing about hidden connections.
	if _, err := store.ExplainAccessAs(ctx, member, granted, access.ResourceConnection, ungranted.ID, owner.PrincipalID); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member asked about another principal: %v", err)
	}
	if got, err := store.ExplainAccessAs(ctx, member, granted, access.ResourceConnection, ungranted.ID, ""); err != nil || got.Usable || len(got.Steps) != 0 {
		t.Fatalf("hidden connection %+v %v", got, err)
	}
}
