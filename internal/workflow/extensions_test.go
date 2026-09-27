package workflow

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/extension/fixture"
	"github.com/mjtechguy/blaxsmith/internal/guild"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

const guildExtensionURL = "https://github.com/alphabravo-oss/guild"

func guildOverlay(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../examples/extensions/guild/blaxsmith-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// requiredGuildPermissions approves everything the Guild manifest requires
// plus the Foundry MCP server; the optional Serena hook and server are left
// out unless extra names them.
func requiredGuildPermissions(t *testing.T, extra ...string) []string {
	t.Helper()
	m, perr := extension.Parse(guildOverlay(t))
	if perr != nil {
		t.Fatal(perr)
	}
	var out []string
	for _, p := range m.Permissions() {
		if !p.Optional || slices.Contains(extra, p.ID) {
			out = append(out, p.ID)
		}
	}
	return out
}

func installGuild(t *testing.T, store *Store, caller identity.Caller, dir, commit string, approved []string) (Extension, ExtensionVersion, error) {
	t.Helper()
	return store.InstallExtensionAs(tenant.System(t.Context()), caller, ExtensionSource{RepositoryURL: guildExtensionURL, GitRef: "main",
		Commit: commit, Directory: dir, Overlay: guildOverlay(t)}, approved)
}

func TestExtensionInstallIsAdminOnlyImmutableAndTracksUpdates(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "ext-install")
	owner := reviewer(t, pool, org, "owner", "ext-install-owner")
	admin := reviewer(t, pool, org, "admin", "ext-install-admin")
	member := reviewer(t, pool, org, "member", "ext-install-member")
	viewer := reviewer(t, pool, org, "viewer", "ext-install-viewer")
	dir, commit := fixture.Repo(t, fixture.GuildFiles)
	approved := requiredGuildPermissions(t, "hook:foundry-serena")

	for _, caller := range []identity.Caller{member, viewer} {
		if _, err := store.PreviewExtension(ctx, caller, ExtensionSource{RepositoryURL: guildExtensionURL, GitRef: "main",
			Commit: commit, Directory: dir, Overlay: guildOverlay(t)}); !errors.Is(err, ErrExtensionDenied) {
			t.Fatalf("%s previewed an install: %v", caller.Role, err)
		}
		if _, _, err := installGuild(t, store, caller, dir, commit, approved); !errors.Is(err, ErrExtensionDenied) {
			t.Fatalf("%s installed an extension: %v", caller.Role, err)
		}
		if _, err := store.RecordExtensionRefAs(ctx, caller, "00000000-0000-0000-0000-000000000000", commit); !errors.Is(err, ErrExtensionDenied) {
			t.Fatalf("%s recorded a ref: %v", caller.Role, err)
		}
	}
	preview, err := store.PreviewExtension(ctx, admin, ExtensionSource{RepositoryURL: guildExtensionURL, GitRef: "main",
		Commit: commit, Directory: dir, Overlay: guildOverlay(t)})
	if err != nil || preview.Manifest.ID != "guild" || len(preview.Permissions) != 12 {
		t.Fatalf("preview: %+v %v", preview, err)
	}
	// A required permission cannot be declined.
	if _, _, err := installGuild(t, store, owner, dir, commit, approved[1:]); !errors.Is(err, extension.ErrApproval) {
		t.Fatalf("install without a required permission: %v", err)
	}
	ext, version, err := installGuild(t, store, owner, dir, commit, approved)
	if err != nil {
		t.Fatal(err)
	}
	if ext.Key != "guild" || ext.CurrentVersion != "1.0.0" || ext.UpdateAvailable() || version.Commit != commit ||
		version.ManifestSHA256 != extension.Digest(guildOverlay(t)) || version.ManifestOrigin != "overlay" ||
		!slices.Contains(version.Approved, "hook:foundry-serena") || version.PermissionsSHA256 != extension.PermissionsDigest(version.Approved) {
		t.Fatalf("installed: %+v %+v", ext, version)
	}
	// Idempotent at the same commit and manifest.
	if _, again, err := installGuild(t, store, admin, dir, commit, approved); err != nil || again.ID != version.ID {
		t.Fatalf("reinstall: %+v %v", again, err)
	}
	// Versions are immutable rows.
	if _, err := pool.Exec(ctx, `UPDATE workflow_extension_versions SET commit_sha=$3 WHERE organization_id=$1 AND id=$2`,
		org, version.ID, strings.Repeat("b", 40)); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("version update: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workflow_extension_versions WHERE organization_id=$1 AND id=$2`, org, version.ID); err == nil {
		t.Fatal("version deleted")
	}
	// The ref moves: update available, and the same version string at a
	// new commit is refused.
	moved := fixture.Commit(t, dir, map[string]string{"README.md": "moved\n"})
	updated, err := store.RecordExtensionRefAs(ctx, admin, ext.ID, moved)
	if err != nil || !updated.UpdateAvailable() || updated.LatestRefCommit != moved {
		t.Fatalf("update check: %+v %v", updated, err)
	}
	if _, _, err := installGuild(t, store, owner, dir, moved, approved); !errors.Is(err, ErrConflict) {
		t.Fatalf("version 1.0.0 reinstalled at a new commit: %v", err)
	}
	// Repository manifests are validated at the commit; a missing one says so.
	if _, err := store.PreviewExtension(ctx, owner, ExtensionSource{RepositoryURL: guildExtensionURL, GitRef: "main",
		Commit: moved, Directory: dir}); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("missing repository manifest: %v", err)
	}
	// A manifest that names content absent at the commit is invalid.
	broken := maps.Clone(fixture.GuildFiles)
	delete(broken, "plugins/foundry/hooks/hooks.json")
	brokenDir, brokenCommit := fixture.Repo(t, broken)
	var invalid *ExtensionInvalidError
	if _, _, err := installGuild(t, store, owner, brokenDir, brokenCommit, approved); !errors.As(err, &invalid) {
		t.Fatalf("broken tree installed: %v", err)
	}
	list, err := store.ListExtensions(ctx, member, "")
	if err != nil || len(list) != 1 || list[0].VersionCount != 1 {
		t.Fatalf("member list: %+v %v", list, err)
	}
	// Scoped to a project, the list holds only extensions usable there.
	project, err := store.CreateProject(ctx, org, "ext-install-project", "Extension project")
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := store.CreateProject(ctx, org, "ext-install-sibling", "Sibling project")
	if err != nil {
		t.Fatal(err)
	}
	if list, err := store.ListExtensions(ctx, member, project); err != nil || len(list) != 0 {
		t.Fatalf("ungranted project list: %+v %v", list, err)
	}
	if _, err := store.GrantExtensionAs(ctx, owner, ext.ID, project, "project", ""); err != nil {
		t.Fatal(err)
	}
	if list, err := store.ListExtensions(ctx, member, project); err != nil || len(list) != 1 || list[0].ID != ext.ID {
		t.Fatalf("granted project list: %+v %v", list, err)
	}
	if list, err := store.ListExtensions(ctx, member, sibling); err != nil || len(list) != 0 {
		t.Fatalf("sibling project list: %+v %v", list, err)
	}
	if list, err := store.ListExtensions(ctx, viewer, project); err != nil || len(list) != 0 {
		t.Fatalf("viewer may not use extensions: %+v %v", list, err)
	}
	if _, err := store.ListExtensions(ctx, member, "not-a-uuid"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed project: %v", err)
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND action='workflow.extension.installed' AND subject_id=$2 AND detail->>'commit'=$3`, org, version.ID, commit).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("install audit events: %d %v", audited, err)
	}
	other := reviewer(t, pool, organization(t, pool, "ext-install-other"), "owner", "ext-install-outsider")
	if _, _, err := store.GetExtension(ctx, other, ext.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other organization read the extension: %v", err)
	}
}

func TestLaunchFreezesExtensionTemplateDigest(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "ext-launch")
	owner := reviewer(t, pool, org, "owner", "ext-launch-owner")
	member := reviewer(t, pool, org, "member", "ext-launch-member")
	project, err := store.CreateProject(ctx, org, "ext-launch", "Extension launch")
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
	// The Guild example recipe with its implement stage filled by the
	// embedded Foundry template on a Claude Code profile.
	data := strings.Replace(string(guildRecipe(t)), `"harness": "codex",
      "model": "gpt-5.6-luna",
      "effort": "xhigh"`, `"harness": "claude-code",
      "model": "claude-opus-4-8",
      "effort": "high"`, 1)
	data = strings.Replace(data, `"prompt": "examples/guild/prompts/implement.md"}`,
		`"prompt": "examples/guild/prompts/implement.md", "template": "guild@1.0.0/foundry-build"}`, 1)
	if !strings.Contains(data, "foundry-build") || !strings.Contains(data, "claude-opus-4-8") {
		t.Fatal("recipe fixture edit did not apply")
	}
	source := recipe.Input{Validators: map[string]recipe.Validator{"guild-forge": guild.ValidateInputs}, Repo: guildRepo(t), Ref: "HEAD", Recipe: "examples/guild/recipe.json",
		Scope:      "examples/guild",
		RecipeData: []byte(data)}
	launch := func(key string) (Run, error) {
		return store.CreateFrozenRun(ctx, FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: key,
			Source: source, Verification: policy, Caller: &member, SourceRepositoryURL: repositoryURL, SourceRef: "main",
			VerificationVersion: verification.Version})
	}
	if _, err := launch("not-installed"); !errors.Is(err, ErrRecipe) || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("launch before install: %v", err)
	}
	dir, commit := fixture.Repo(t, fixture.GuildFiles)
	ext, version, err := installGuild(t, store, owner, dir, commit, requiredGuildPermissions(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := launch("ungranted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("launch without an extension grant: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.GrantResource(ctx, tx, owner, access.ResourceExtension, ext.ID, access.Grantee{ProjectID: project}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	run, err := launch("granted")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.LoadFrozenTask(ctx, org, run.ID, taskID(t, store, org, run.ID, "implement"))
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Bundle.Extensions) != 1 {
		t.Fatalf("bundle extensions: %+v", task.Bundle.Extensions)
	}
	frozen := task.Bundle.Extensions[0]
	if frozen.ID != "guild" || frozen.Version != "1.0.0" || frozen.Commit != commit || frozen.ManifestSHA256 != version.ManifestSHA256 ||
		frozen.PermissionsSHA256 != version.PermissionsSHA256 || task.Stage.Template != "guild@1.0.0/foundry-build" {
		t.Fatalf("frozen extension: %+v stage %+v", frozen, task.Stage)
	}
	if _, err := frozen.Load(); err != nil {
		t.Fatalf("frozen manifest: %v", err)
	}
	if required, err := GateDeclaration(task, "foundry-assay", "pass"); err != nil || required {
		t.Fatalf("informational gate: %v %v", required, err)
	}
	if _, err := GateDeclaration(task, "undeclared", "pass"); err == nil {
		t.Fatal("undeclared gate accepted")
	}
	// Make the plan succeeded so the implement fixture can exercise live gates.
	if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded' WHERE organization_id=$1 AND run_id=$2 AND task_key='plan'`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	a, err := store.ReserveAttempt(ctx, org, run.ID, task.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ConfirmStarting(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = store.ConfirmStarted(ctx, a); err != nil {
		t.Fatal(err)
	}
	gate := evidence.Gate{ID: "assay-1", Check: "foundry-assay", Verdict: "fail", Summary: "A check failed."}
	receipt, err := store.RecordGate(ctx, a, run.SourceCommit, gate, "")
	if err != nil || !receipt.Accepted {
		t.Fatalf("gate record: %+v %v", receipt, err)
	}
	if replay, found, err := store.GateReceipt(ctx, a, gate); err != nil || !found || replay != receipt {
		t.Fatalf("gate replay: %+v %v %v", replay, found, err)
	}
	gate.Verdict = "pass"
	if replay, found, err := store.GateReceipt(ctx, a, gate); err != nil || !found || replay.Accepted {
		t.Fatalf("changed gate replay: %+v %v %v", replay, found, err)
	}
	gate.ID = "assay-missing"
	gate.Check = "undeclared"
	if receipt, err := store.RecordGate(ctx, a, run.SourceCommit, gate, ""); err != nil || receipt.Accepted {
		t.Fatalf("unknown gate: %+v %v", receipt, err)
	}
	recorded, err := store.RunExtensions(ctx, org, run.ID)
	if err != nil || len(recorded) != 1 || recorded[0].ID != version.ID {
		t.Fatalf("run provenance: %+v %v", recorded, err)
	}
	// The bundle digest covers the extension: the same recipe without the
	// template freezes differently.
	source.RecipeData = []byte(strings.Replace(data, `, "template": "guild@1.0.0/foundry-build"`, "", 1))
	plain, err := launch("plain")
	if err != nil {
		t.Fatal(err)
	}
	if plain.BundleSHA256 == run.BundleSHA256 {
		t.Fatal("extension did not change the bundle digest")
	}
	// A recipe can require an extension gate without inventing a platform
	// command with the same id. Missing, failed and stale submissions fail closed.
	source.RecipeData = []byte(strings.Replace(data, `"required_checks": ["project-tests", "requirement-coverage"]`, `"required_checks": ["project-tests", "requirement-coverage", "foundry-assay"]`, 1))
	gated, err := launch("required-gate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET state='succeeded' WHERE organization_id=$1 AND run_id=$2 AND task_key='plan'`, org, gated.ID); err != nil {
		t.Fatal(err)
	}
	ga, err := store.ReserveAttempt(ctx, org, gated.ID, taskID(t, store, org, gated.ID, "implement"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.ConfirmStarting(ctx, ga); err != nil {
		t.Fatal(err)
	}
	if err = store.ConfirmStarted(ctx, ga); err != nil {
		t.Fatal(err)
	}
	assertGate := func(revision string, want bool) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		err = requiredGates(ctx, tx, ga, revision)
		if (err == nil) != want {
			t.Fatalf("required gate readiness=%v err=%v", want, err)
		}
	}
	assertGate(gated.SourceCommit, false)
	pass := evidence.Gate{ID: "required-pass", Check: "foundry-assay", Verdict: "pass", Summary: "Pass"}
	if receipt, err := store.RecordGate(ctx, ga, gated.SourceCommit, pass, ""); err != nil || !receipt.Accepted {
		t.Fatalf("required pass: %+v %v", receipt, err)
	}
	assertGate(gated.SourceCommit, true)
	assertGate(strings.Repeat("f", 40), false)
	pass.ID = "required-fail"
	pass.Verdict = "fail"
	if _, err := store.RecordGate(ctx, ga, gated.SourceCommit, pass, ""); err != nil {
		t.Fatal(err)
	}
	assertGate(gated.SourceCommit, false)
	// A template on the wrong harness is refused at freeze.
	source.RecipeData = []byte(strings.Replace(string(guildRecipe(t)), `"prompt": "examples/guild/prompts/implement.md"}`,
		`"prompt": "examples/guild/prompts/implement.md", "template": "guild@1.0.0/foundry-build"}`, 1))
	if _, err := launch("wrong-harness"); !errors.Is(err, ErrRecipe) {
		t.Fatalf("codex profile ran a Claude Code template: %v", err)
	}
}

func taskID(t *testing.T, store *Store, org, runID, key string) string {
	t.Helper()
	var id string
	if err := store.pool.QueryRow(tenant.System(context.Background()), `SELECT id::text FROM workflow_tasks WHERE organization_id=$1 AND run_id=$2 AND task_key=$3`,
		org, runID, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}
