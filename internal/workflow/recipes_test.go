package workflow

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/guild"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// grantRecipe records an organization recipe grant through lane U's
// access.GrantResource and returns the grant ID.
func grantRecipe(t *testing.T, store *Store, owner identity.Caller, recipeID string, to access.Grantee) string {
	t.Helper()
	tx, err := store.pool.Begin(tenant.System(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(tenant.System(context.Background()))
	id, err := access.GrantResource(tenant.System(t.Context()), tx, owner, access.ResourceRecipe, recipeID, to)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(tenant.System(t.Context())); err != nil {
		t.Fatal(err)
	}
	return id
}

// seedGrants lists live member-role grants on an organization recipe.
func seedGrants(t *testing.T, store *Store, org, recipeID string) []string {
	t.Helper()
	rows, err := store.pool.Query(tenant.System(t.Context()), `SELECT id::text FROM access_resource_grants WHERE organization_id=$1
		AND resource_kind='recipe' AND resource_id=$2 AND grantee_role='member' AND revoked_at IS NULL`, org, recipeID)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return ids
}

func revokeRecipeGrant(t *testing.T, store *Store, owner identity.Caller, grantID string) {
	t.Helper()
	tx, err := store.pool.Begin(tenant.System(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(tenant.System(context.Background()))
	if err := access.RevokeResourceGrant(tenant.System(t.Context()), tx, owner, grantID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(tenant.System(t.Context())); err != nil {
		t.Fatal(err)
	}
}

func guildRecipe(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../examples/guild/recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func seededAnvil(t *testing.T, store *Store, caller identity.Caller) (LibraryRecipe, RecipeVersion) {
	t.Helper()
	recipes, err := store.ListRecipes(tenant.System(t.Context()), caller, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range recipes {
		if r.Name == "Anvil starter" {
			v, err := store.GetRecipeVersion(tenant.System(t.Context()), caller, r.CurrentVersionID)
			if err != nil {
				t.Fatal(err)
			}
			return r, v
		}
	}
	t.Fatalf("Anvil starter seed missing: %+v", recipes)
	return LibraryRecipe{}, RecipeVersion{}
}

func TestRecipeSeedIsIdempotentAndVersionsAreImmutable(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "recipe-seed")
	owner := reviewer(t, pool, org, "owner", "recipe-seed-owner")
	seed, version := seededAnvil(t, store, owner)
	if seed.ProjectID != "" || seed.CurrentVersion != 1 || version.FrozenPath != "examples/anvil/recipe.json" ||
		string(version.JSON) != string(anvilRecipe(t)) || version.SHA256 != sha(anvilRecipe(t)) || version.AuthorID != "" {
		t.Fatalf("seed does not match examples/anvil/recipe.json: %+v %+v", seed, version)
	}
	if _, _, fe := recipe.Validate(version.JSON); fe != nil {
		t.Fatalf("seed is invalid: %v", fe)
	}
	// Re-running the seed and the migrations adds nothing.
	for range 2 {
		if _, err := pool.Exec(ctx, `SELECT workflow_seed_recipes($1)`, org); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := pool.Exec(ctx, `SELECT workflow_seed_recipe_grants($1)`, org); err != nil {
			t.Fatal(err)
		}
	}
	grants := seedGrants(t, store, org, seed.ID)
	if len(grants) != 1 {
		t.Fatalf("seeded recipe member grants: %v", grants)
	}
	project, err := store.CreateProject(ctx, org, "recipe-seed-project", "Recipe seed project")
	if err != nil {
		t.Fatal(err)
	}
	member := reviewer(t, pool, org, "member", "recipe-seed-member")
	viewer := reviewer(t, pool, org, "viewer", "recipe-seed-project-viewer")
	if list, err := store.ListRecipes(ctx, member, project); err != nil || len(list) != 1 || list[0].ID != seed.ID {
		t.Fatalf("member cannot use the seeded recipe by default: %+v %v", list, err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, project, version.ID); err != nil {
		t.Fatalf("seeded recipe not launchable by default: %v", err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, viewer, project, version.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("viewer launched the seeded recipe: %v", err)
	}
	// A revoked default grant stays revoked when the seed runs again.
	revokeRecipeGrant(t, store, owner, grants[0])
	if _, err := pool.Exec(ctx, `SELECT workflow_seed_recipe_grants($1)`, org); err != nil {
		t.Fatal(err)
	}
	if again := seedGrants(t, store, org, seed.ID); len(again) != 0 {
		t.Fatalf("seed restored a revoked grant: %v", again)
	}
	if list, err := store.ListRecipes(ctx, member, project); err != nil || len(list) != 0 {
		t.Fatalf("revoked seeded recipe still usable: %+v %v", list, err)
	}
	var recipes, versions int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_recipes WHERE organization_id=$1),
		(SELECT count(*) FROM workflow_recipe_versions WHERE organization_id=$1)`, org).Scan(&recipes, &versions); err != nil ||
		recipes != 1 || versions != 1 {
		t.Fatalf("seed not idempotent: %d recipes, %d versions, %v", recipes, versions, err)
	}

	edited := []byte(strings.Replace(string(anvilRecipe(t)), `"timeout_seconds": 1800`, `"timeout_seconds": 1200`, 1))
	second, err := store.CreateRecipeVersionAs(ctx, owner, seed.ID, edited, "", false)
	if err != nil || second.Version != 2 || second.FrozenPath != ".blaxsmith/recipes/anvil-starter.json" || second.AuthorID != owner.PrincipalID {
		t.Fatalf("second version: %+v %v", second, err)
	}
	if current, _, err := store.GetRecipe(ctx, owner, seed.ID); err != nil || current.CurrentVersionID != version.ID || current.VersionCount != 2 {
		t.Fatalf("unmarked version became current: %+v %v", current, err)
	}
	if err := store.SetCurrentRecipeVersionAs(ctx, owner, seed.ID, second.ID); err != nil {
		t.Fatal(err)
	}
	if current, _, err := store.GetRecipe(ctx, owner, seed.ID); err != nil || current.CurrentVersion != 2 {
		t.Fatalf("current not set: %+v %v", current, err)
	}
	for _, statement := range []string{
		`UPDATE workflow_recipe_versions SET recipe_json=convert_to('{}','UTF8'),sha256=encode(sha256(convert_to('{}','UTF8')),'hex') WHERE organization_id=$1 AND id=$2`,
		`UPDATE workflow_recipe_versions SET frozen_path='other.json' WHERE organization_id=$1 AND id=$2`,
		`DELETE FROM workflow_recipe_versions WHERE organization_id=$1 AND id=$2`,
	} {
		if _, err := pool.Exec(ctx, statement, org, version.ID); err == nil || !strings.Contains(err.Error(), "immutable") {
			t.Fatalf("version mutated: %s: %v", statement, err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_recipe_versions (organization_id,recipe_id,version,recipe_json,sha256,frozen_path)
		VALUES ($1,$2,9,convert_to('{}','UTF8'),$3,'x.json')`, org, seed.ID, strings.Repeat("0", 64)); err == nil {
		t.Fatal("stored a version whose digest does not match its bytes")
	}
	again, err := store.GetRecipeVersion(ctx, owner, version.ID)
	if err != nil || string(again.JSON) != string(version.JSON) || again.SHA256 != version.SHA256 {
		t.Fatalf("version 1 changed: %v", err)
	}
	// A new organization is seeded by the trigger.
	later := organization(t, pool, "recipe-seed-later")
	laterSeed, v := seededAnvil(t, store, reviewer(t, pool, later, "viewer", "recipe-seed-viewer"))
	if v.SHA256 != version.SHA256 {
		t.Fatal("later organization seeded different bytes")
	}
	if grants := seedGrants(t, store, later, laterSeed.ID); len(grants) != 1 {
		t.Fatalf("later organization's seeded recipe not granted to members: %v", grants)
	}
}

func TestRecipeValidationErrorsAreSurfaced(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "recipe-invalid")
	owner := reviewer(t, pool, org, "owner", "recipe-invalid-owner")
	bad := []byte(strings.Replace(string(guildRecipe(t)), `"max_cycles": 3`, `"max_cycles": 99`, 1))
	var invalid *RecipeInvalidError
	if _, _, err := store.CreateRecipeAs(ctx, owner, "", "Broken", "", bad, ""); !errors.As(err, &invalid) ||
		invalid.Field.Path != "stages[3].loop" || !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid recipe error: %v", err)
	}
	if _, _, err := store.CreateRecipeAs(ctx, owner, "", "Broken", "", []byte(`{"schema_version":`), ""); !errors.As(err, &invalid) ||
		invalid.Field.Path != "$" || !strings.Contains(invalid.Field.Message, "line 1") {
		t.Fatalf("syntax error: %v", err)
	}
	if _, _, err := store.CreateRecipeAs(ctx, owner, "", "Broken", "", guildRecipe(t), "../escape.json"); !errors.As(err, &invalid) ||
		invalid.Field.Path != "frozen_path" {
		t.Fatalf("path label error: %v", err)
	}
	seed, _ := seededAnvil(t, store, owner)
	if _, err := store.CreateRecipeVersionAs(ctx, owner, seed.ID, bad, "", true); !errors.As(err, &invalid) {
		t.Fatalf("invalid version accepted: %v", err)
	}
	if _, _, err := store.CreateRecipeAs(ctx, owner, "", "anvil STARTER", "", guildRecipe(t), ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate scoped name: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_recipe_versions WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 1 {
		t.Fatalf("invalid input stored versions: %d %v", count, err)
	}
}

func TestRecipeScopesAndPermissions(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "recipe-scope")
	other := organization(t, pool, "recipe-scope-other")
	owner := reviewer(t, pool, org, "owner", "recipe-scope-owner")
	admin := reviewer(t, pool, org, "admin", "recipe-scope-admin")
	member := reviewer(t, pool, org, "member", "recipe-scope-member")
	viewer := reviewer(t, pool, org, "viewer", "recipe-scope-viewer")
	outsider := reviewer(t, pool, other, "owner", "recipe-scope-outsider")
	project, err := store.CreateProject(ctx, org, "recipe-project", "Recipe project")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := store.CreateProject(ctx, org, "recipe-sibling", "Sibling project")
	if err != nil {
		t.Fatal(err)
	}
	data := guildRecipe(t)
	// Members and viewers read; only owners/admins edit either scope.
	for _, caller := range []identity.Caller{member, viewer} {
		if list, err := store.ListRecipes(ctx, caller, ""); err != nil || len(list) != 1 {
			t.Fatalf("%s read: %+v %v", caller.Role, list, err)
		}
		if _, _, err := store.CreateRecipeAs(ctx, caller, "", "Org "+caller.Role, "", data, ""); !errors.Is(err, ErrRecipeDenied) {
			t.Fatalf("%s created org recipe: %v", caller.Role, err)
		}
		if _, _, err := store.CreateRecipeAs(ctx, caller, project, "Project "+caller.Role, "", data, ""); !errors.Is(err, ErrRecipeDenied) {
			t.Fatalf("%s created project recipe: %v", caller.Role, err)
		}
	}
	orgRecipe, orgVersion, err := store.CreateRecipeAs(ctx, owner, "", "Org fast", "Fast loop", data, "")
	if err != nil || orgRecipe.ProjectID != "" || orgRecipe.CurrentVersionID != orgVersion.ID {
		t.Fatalf("owner org recipe: %+v %v", orgRecipe, err)
	}
	orgFastGrants := []string{grantRecipe(t, store, owner, orgRecipe.ID, access.Grantee{ProjectID: project}),
		grantRecipe(t, store, owner, orgRecipe.ID, access.Grantee{ProjectID: sibling})}
	projectRecipe, projectVersion, err := store.CreateRecipeAs(ctx, admin, project, "Org fast", "Same name, project scope", data, "")
	if err != nil || projectRecipe.ProjectID != project {
		t.Fatalf("admin project recipe: %+v %v", projectRecipe, err)
	}
	if _, err := store.CreateRecipeVersionAs(ctx, member, projectRecipe.ID, data, "", true); !errors.Is(err, ErrRecipeDenied) {
		t.Fatalf("member versioned project recipe: %v", err)
	}
	if err := store.SetCurrentRecipeVersionAs(ctx, viewer, orgRecipe.ID, orgVersion.ID); !errors.Is(err, ErrRecipeDenied) {
		t.Fatalf("viewer set current: %v", err)
	}
	// A token whose role was downgraded is fenced under lock.
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='member' WHERE organization_id=$1 AND principal_id=$2`, org, admin.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRecipeVersionAs(ctx, admin, orgRecipe.ID, data, "", true); !errors.Is(err, ErrFenced) {
		t.Fatalf("downgraded admin edited: %v", err)
	}

	names := func(list []LibraryRecipe) []string {
		var out []string
		for _, r := range list {
			out = append(out, r.Name+"@"+map[bool]string{true: "org", false: "project"}[r.ProjectID == ""])
		}
		return out
	}
	inProject, err := store.ListRecipes(ctx, member, project)
	if err != nil || strings.Join(names(inProject), ",") != "Org fast@project,Anvil starter@org,Org fast@org" {
		t.Fatalf("project library: %v %v", names(inProject), err)
	}
	inSibling, err := store.ListRecipes(ctx, member, sibling)
	if err != nil || strings.Join(names(inSibling), ",") != "Anvil starter@org,Org fast@org" {
		t.Fatalf("sibling sees project recipe: %v %v", names(inSibling), err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, sibling, projectVersion.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("launched another project's recipe: %v", err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, sibling, orgVersion.ID); err != nil {
		t.Fatalf("granted org recipe not launchable: %v", err)
	}
	// Organization isolation.
	if _, _, err := store.GetRecipe(ctx, outsider, orgRecipe.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider read recipe: %v", err)
	}
	if _, err := store.GetRecipeVersion(ctx, outsider, orgVersion.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider read version: %v", err)
	}
	if list, err := store.ListRecipes(ctx, outsider, project); !errors.Is(err, ErrNotFound) {
		t.Fatalf("outsider listed project: %v %v", list, err)
	}
	// Clone an org recipe into a project, then without the grant.
	clone, cloneVersion, err := store.CloneRecipeAs(ctx, owner, orgVersion.ID, sibling, "Sibling copy", "")
	if err != nil || clone.ProjectID != sibling || cloneVersion.SHA256 != orgVersion.SHA256 || cloneVersion.FrozenPath != orgVersion.FrozenPath {
		t.Fatalf("clone: %+v %+v %v", clone, cloneVersion, err)
	}
	if _, _, err := store.CloneRecipeAs(ctx, owner, projectVersion.ID, sibling, "Stolen", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cloned another project's recipe: %v", err)
	}
	for _, id := range orgFastGrants {
		revokeRecipeGrant(t, store, owner, id)
	}
	if list, err := store.ListRecipes(ctx, member, project); err != nil || strings.Join(names(list), ",") != "Org fast@project,Anvil starter@org" {
		t.Fatalf("ungranted org recipes visible: %v %v", names(list), err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, project, orgVersion.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted org recipe launchable: %v", err)
	}
	if _, _, err := store.CloneRecipeAs(ctx, owner, orgVersion.ID, project, "Ungranted copy", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted org recipe cloned: %v", err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, project, projectVersion.ID); err != nil {
		t.Fatalf("own project recipe needs no grant: %v", err)
	}
}

func TestLaunchFromLibraryFreezesSameDigestAsFile(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "recipe-launch")
	owner := reviewer(t, pool, org, "owner", "recipe-launch-owner")
	member := reviewer(t, pool, org, "member", "recipe-launch-member")
	project, err := store.CreateProject(ctx, org, "recipe-launch", "Recipe launch")
	if err != nil {
		t.Fatal(err)
	}
	const repositoryURL = "https://github.com/example/recipes.git"
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_project_sources (organization_id,project_id,repository_url,git_ref)
		VALUES ($1,$2,$3,'main')`, org, project, repositoryURL); err != nil {
		t.Fatal(err)
	}
	policy := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{
		{ID: "project-tests", Command: []string{"go", "test", "./..."}},
		{ID: "requirement-coverage", Command: []string{"verify-coverage"}},
	}}
	verification, err := store.SetProjectVerificationAs(ctx, owner, project, 0, policy)
	if err != nil {
		t.Fatal(err)
	}
	source := recipe.Input{Validators: map[string]recipe.Validator{"guild-forge": guild.ValidateInputs}, Repo: anvilRepo(t), Ref: "HEAD", Recipe: "examples/anvil/recipe.json",
		Scope: "examples/anvil"}
	launch := func(key string, source recipe.Input, versionID string) (Run, error) {
		return store.CreateFrozenRun(ctx, FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: key,
			Source: source, Verification: policy, Caller: &member, SourceRepositoryURL: repositoryURL, SourceRef: "main",
			VerificationVersion: verification.Version, RecipeVersionID: versionID})
	}
	fromFile, err := launch("from-file", source, "")
	if err != nil {
		t.Fatal(err)
	}
	guild, seed := seededAnvil(t, store, member)
	guildGrants := seedGrants(t, store, org, guild.ID) // Seeded: no explicit grant needed.
	library, err := store.LibraryRecipeForLaunch(ctx, member, project, seed.ID)
	if err != nil {
		t.Fatal(err)
	}
	fromLibrary := source
	fromLibrary.Recipe, fromLibrary.RecipeData = library.FrozenPath, library.JSON
	run, err := launch("from-library", fromLibrary, library.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.BundleSHA256 != fromFile.BundleSHA256 || run.SourceCommit != fromFile.SourceCommit {
		t.Fatalf("library launch froze %s, file launch froze %s", run.BundleSHA256, fromFile.BundleSHA256)
	}
	var recorded string
	var bundleRecipe string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(r.recipe_version_id::text,''),b.bundle_json->'source'->>'recipe'
		FROM workflow_runs r JOIN workflow_run_bundles b ON b.organization_id=r.organization_id AND b.run_id=r.id
		WHERE r.organization_id=$1 AND r.id=$2`, org, run.ID).Scan(&recorded, &bundleRecipe); err != nil ||
		recorded != library.ID || bundleRecipe != "examples/anvil/recipe.json" {
		t.Fatalf("library provenance: %q %q %v", recorded, bundleRecipe, err)
	}
	if tasks, err := store.ListRunTasks(ctx, org, run.ID); err != nil || len(tasks) != 2 {
		t.Fatalf("library run graph: %d %v", len(tasks), err)
	}
	// Bytes that differ from the named version are refused at admission.
	tampered := fromLibrary
	tampered.RecipeData = []byte(strings.Replace(string(library.JSON), `"timeout_seconds": 1800`, `"timeout_seconds": 1200`, 1))
	if _, err := launch("tampered", tampered, library.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("tampered library bytes launched: %v", err)
	}
	for _, id := range guildGrants {
		revokeRecipeGrant(t, store, owner, id)
	}
	if _, err := launch("ungranted", fromLibrary, library.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted library recipe launched: %v", err)
	}
}

func TestRecipeGrantsAreManagedAndAudited(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "recipe-grants")
	owner := reviewer(t, pool, org, "owner", "recipe-grants-owner")
	member := reviewer(t, pool, org, "member", "recipe-grants-member")
	project, err := store.CreateProject(ctx, org, "recipe-grants", "Recipe grants")
	if err != nil {
		t.Fatal(err)
	}
	orgRecipe, orgVersion, err := store.CreateRecipeAs(ctx, owner, "", "Granted", "", guildRecipe(t), "")
	if err != nil {
		t.Fatal(err)
	}
	projectRecipe, _, err := store.CreateRecipeAs(ctx, owner, project, "Local", "", guildRecipe(t), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, project, orgVersion.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted recipe launchable: %v", err)
	}
	if _, err := store.GrantRecipeAs(ctx, member, orgRecipe.ID, project, "project", ""); !errors.Is(err, ErrRecipeDenied) {
		t.Fatalf("member granted a recipe: %v", err)
	}
	if _, err := store.GrantRecipeAs(ctx, owner, projectRecipe.ID, project, "project", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("granted a project recipe: %v", err)
	}
	if _, err := store.GrantRecipeAs(ctx, owner, orgRecipe.ID, "", "role", "viewer"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("granted to viewers: %v", err)
	}
	grant, err := store.GrantRecipeAs(ctx, owner, orgRecipe.ID, project, "project", "")
	if err != nil || grant.GranteeKind != "project" || grant.ProjectID != project || grant.ProjectName != "Recipe grants" {
		t.Fatalf("project grant: %+v %v", grant, err)
	}
	if again, err := store.GrantRecipeAs(ctx, owner, orgRecipe.ID, project, "project", ""); err != nil || again.ID != grant.ID {
		t.Fatalf("regrant not idempotent: %+v %v", again, err)
	}
	userGrant, err := store.GrantRecipeAs(ctx, owner, orgRecipe.ID, "", "user", member.PrincipalID)
	if err != nil || userGrant.GranteeKind != "user" || userGrant.GranteeName != "recipe-grants-member" {
		t.Fatalf("user grant: %+v %v", userGrant, err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, project, orgVersion.ID); err != nil {
		t.Fatalf("granted recipe not launchable: %v", err)
	}
	if grants, err := store.RecipeGrants(ctx, owner, orgRecipe.ID); err != nil || len(grants) != 2 {
		t.Fatalf("owner grant list: %+v %v", grants, err)
	}
	if grants, err := store.RecipeGrants(ctx, member, orgRecipe.ID); err != nil || len(grants) != 0 {
		t.Fatalf("member saw grants: %+v %v", grants, err)
	}
	if err := store.RevokeRecipeGrantAs(ctx, member, grant.ID); !errors.Is(err, ErrRecipeDenied) {
		t.Fatalf("member revoked: %v", err)
	}
	for _, id := range []string{grant.ID, userGrant.ID} {
		if err := store.RevokeRecipeGrantAs(ctx, owner, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RevokeRecipeGrantAs(ctx, owner, grant.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked twice: %v", err)
	}
	if _, err := store.LibraryRecipeForLaunch(ctx, member, project, orgVersion.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked recipe launchable: %v", err)
	}
	var created, revoked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE action='access.resource_grant.created'),
		count(*) FILTER (WHERE action='access.resource_grant.revoked') FROM identity_audit_events
		WHERE organization_id=$1 AND actor_id=$2`, org, owner.PrincipalID).Scan(&created, &revoked); err != nil || created != 2 || revoked != 2 {
		t.Fatalf("grant audit: %d created, %d revoked, %v", created, revoked, err)
	}
}

func anvilRecipe(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../examples/anvil/recipe.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}
