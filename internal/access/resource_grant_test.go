package access

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func grantOrg(t *testing.T, pool *pgxpool.Pool, slug string) (org, project string) {
	t.Helper()
	ctx := tenant.System(t.Context())
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name) VALUES (gen_random_uuid(),$1,$1) RETURNING id`,
		slug).Scan(&org); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_projects (organization_id,slug,name) VALUES ($1,$2,$2) RETURNING id`,
		org, slug+"-proj").Scan(&project); err != nil {
		t.Fatal(err)
	}
	return org, project
}

func grantMember(t *testing.T, pool *pgxpool.Pool, org, username, role string) identity.Caller {
	t.Helper()
	var id string
	if err := pool.QueryRow(tenant.System(t.Context()), `INSERT INTO identity_principals (id,username) VALUES (gen_random_uuid(),$1) RETURNING id`,
		username).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(tenant.System(t.Context()), `INSERT INTO identity_memberships (organization_id,principal_id,role) VALUES ($1,$2,$3)`,
		org, id, role); err != nil {
		t.Fatal(err)
	}
	return identity.Caller{OrganizationID: org, PrincipalID: id, Role: role}
}

func TestCanUseResourceGrantsPostgres(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(context.Background())
	org, project := grantOrg(t, pool, "grants-a")
	other, otherProject := grantOrg(t, pool, "grants-b")
	owner := grantMember(t, pool, org, "g-owner", "owner")
	admin := grantMember(t, pool, org, "g-admin", "admin")
	member := grantMember(t, pool, org, "g-member", "member")
	second := grantMember(t, pool, org, "g-second", "member")
	viewer := grantMember(t, pool, org, "g-viewer", "viewer")
	outsider := grantMember(t, pool, other, "g-outsider", "owner")

	can := func(who identity.Caller, projectID, kind, id string) bool {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		ok, err := CanUse(ctx, tx, who, projectID, kind, id)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	grant := func(by identity.Caller, kind, id string, to Grantee) (string, error) {
		t.Helper()
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx)
		grantID, err := GrantResource(ctx, tx, by, kind, id, to)
		if err == nil {
			err = tx.Commit(ctx)
		}
		return grantID, err
	}

	// Nothing is usable without a grant, including for owners.
	if can(owner, project, ResourceConnection, "conn-1") {
		t.Fatal("owner used an ungranted connection")
	}
	// Only an owner or admin can grant.
	if _, err := grant(member, ResourceConnection, "conn-1", Grantee{ProjectID: project}); !errors.Is(err, ErrDenied) {
		t.Fatalf("member granted: %v", err)
	}
	// Project grant: usable in that project only.
	projectGrant, err := grant(admin, ResourceConnection, "conn-1", Grantee{ProjectID: project})
	if err != nil {
		t.Fatal(err)
	}
	if again, err := grant(admin, ResourceConnection, "conn-1", Grantee{ProjectID: project}); err != nil || again != projectGrant {
		t.Fatalf("duplicate grant: %q %v", again, err)
	}
	if !can(member, project, ResourceConnection, "conn-1") || can(member, "", ResourceConnection, "conn-1") {
		t.Fatal("project grant scope is wrong")
	}
	if can(viewer, project, ResourceConnection, "conn-1") {
		t.Fatal("viewer used a resource")
	}
	if can(member, project, ResourceRecipe, "conn-1") {
		t.Fatal("grant crossed resource kinds")
	}
	// User grant: that member only, with or without a project.
	if _, err := grant(owner, ResourceRecipe, "recipe-1", Grantee{PrincipalID: member.PrincipalID}); err != nil {
		t.Fatal(err)
	}
	if !can(member, "", ResourceRecipe, "recipe-1") || !can(member, project, ResourceRecipe, "recipe-1") ||
		can(second, "", ResourceRecipe, "recipe-1") {
		t.Fatal("user grant scope is wrong")
	}
	// Role grant: that role and above.
	if _, err := grant(owner, ResourceRecipe, "recipe-2", Grantee{Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	if !can(admin, "", ResourceRecipe, "recipe-2") || !can(owner, "", ResourceRecipe, "recipe-2") ||
		can(member, "", ResourceRecipe, "recipe-2") {
		t.Fatal("role grant scope is wrong")
	}
	// Cross-organization: grants never match, and cross-org grantees are refused.
	if can(outsider, project, ResourceConnection, "conn-1") || can(outsider, "", ResourceRecipe, "recipe-2") {
		t.Fatal("outsider used another organization's grant")
	}
	foreign := outsider
	foreign.OrganizationID = org
	if can(foreign, project, ResourceConnection, "conn-1") {
		t.Fatal("non-member claimed another organization")
	}
	if _, err := grant(owner, ResourceConnection, "conn-2", Grantee{ProjectID: otherProject}); !errors.Is(err, ErrResourceGrantInvalid) {
		t.Fatalf("cross-org project grantee: %v", err)
	}
	if _, err := grant(owner, ResourceConnection, "conn-2", Grantee{PrincipalID: outsider.PrincipalID}); !errors.Is(err, ErrResourceGrantInvalid) {
		t.Fatalf("cross-org member grantee: %v", err)
	}
	if _, err := grant(outsider, ResourceConnection, "conn-1", Grantee{ProjectID: project}); !errors.Is(err, ErrResourceGrantInvalid) {
		t.Fatalf("outsider granted into another org: %v", err)
	}
	for _, bad := range []Grantee{{}, {Role: "viewer"}, {ProjectID: project, Role: "member"}, {PrincipalID: "nope"}} {
		if _, err := grant(owner, ResourceConnection, "conn-3", bad); !errors.Is(err, ErrResourceGrantInvalid) {
			t.Fatalf("invalid grantee %+v: %v", bad, err)
		}
	}
	if _, err := grant(owner, "secret", "x", Grantee{Role: "member"}); !errors.Is(err, ErrResourceGrantInvalid) {
		t.Fatalf("unknown kind: %v", err)
	}
	if can(member, "not-a-uuid", ResourceConnection, "conn-1") || can(member, project, "secret", "conn-1") {
		t.Fatal("malformed input allowed")
	}
	// Disabled members and revoked grants deny.
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET state='disabled' WHERE organization_id=$1 AND principal_id=$2`,
		org, member.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if can(member, project, ResourceConnection, "conn-1") {
		t.Fatal("disabled member used a resource")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RevokeResourceGrant(ctx, tx, admin, projectGrant); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if can(second, project, ResourceConnection, "conn-1") {
		t.Fatal("revoked grant still usable")
	}
	var audited int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events WHERE organization_id=$1
		AND action IN ('access.resource_grant.created','access.resource_grant.revoked')`, org).Scan(&audited); err != nil || audited != 4 {
		t.Fatalf("audit events: %d %v", audited, err)
	}
}
