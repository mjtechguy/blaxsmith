package workflow

import (
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

func TestProjectModelAccessCommitsAuthorityAndSecretTogether(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "model-access")
	project, err := store.CreateProject(t.Context(), org, "model-access", "Model access")
	if err != nil {
		t.Fatal(err)
	}
	owner := reviewer(t, pool, org, "owner", "model-owner")
	viewer := reviewer(t, pool, org, "viewer", "model-viewer")
	secrets, err := access.NewSecretStore(pool, "primary", map[string][]byte{"primary": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	key := []byte("test-provider-secret")
	item, err := store.CreateProjectModelAccessAs(t.Context(), owner, project, "openai", "gpt-5", key, secrets)
	if err != nil || item.ConnectionID == "" || item.GrantID == "" || item.ID == "" {
		t.Fatalf("create: %+v, %v", item, err)
	}
	items, err := store.ListProjectModelAccess(t.Context(), org, project)
	if err != nil || len(items) != 1 || items[0] != item {
		t.Fatalf("list: %+v, %v", items, err)
	}
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	secret, err := secrets.ReadCurrent(t.Context(), tx, org, item.ConnectionID)
	if err != nil || string(secret.Bytes) != string(key) {
		t.Fatalf("secret custody: %v", err)
	}
	secret.Clear()
	if _, err := store.CreateProjectModelAccessAs(t.Context(), owner, project, "openai", "gpt-5", key, secrets); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate selection: %v", err)
	}
	var connections int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM access_connections WHERE organization_id=$1`, org).Scan(&connections); err != nil || connections != 1 {
		t.Fatalf("duplicate left a connection: %d, %v", connections, err)
	}
	if _, err := store.CreateProjectModelAccessAs(t.Context(), viewer, project, "anthropic", "claude-test", key, secrets); !errors.Is(err, ErrProjectModelAccessDenied) {
		t.Fatalf("viewer created model access: %v", err)
	}
}

func TestProjectModelAccessRevocationIsScopedAndRepeatable(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "model-access-revoke")
	project, err := store.CreateProject(t.Context(), org, "model-access-revoke", "Model access revoke")
	if err != nil {
		t.Fatal(err)
	}
	owner := reviewer(t, pool, org, "owner", "model-revoke-owner")
	viewer := reviewer(t, pool, org, "viewer", "model-revoke-viewer")
	secrets, err := access.NewSecretStore(pool, "primary", map[string][]byte{"primary": []byte(strings.Repeat("k", 32))})
	if err != nil {
		t.Fatal(err)
	}
	item, err := store.CreateProjectModelAccessAs(t.Context(), owner, project, "openai", "gpt-5", []byte("test-provider-secret"), secrets)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeProjectModelAccessAs(t.Context(), viewer, item.ID); !errors.Is(err, ErrProjectModelAccessDenied) {
		t.Fatalf("viewer revoked model access: %v", err)
	}
	if err := store.RevokeProjectModelAccessAs(t.Context(), owner, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeProjectModelAccessAs(t.Context(), owner, item.ID); err != nil {
		t.Fatalf("repeated revocation: %v", err)
	}
	items, err := store.ListProjectModelAccess(t.Context(), org, project)
	if err != nil || len(items) != 0 {
		t.Fatalf("revoked selection remains available: %+v, %v", items, err)
	}
	var grantRevoked bool
	var connectionState string
	err = pool.QueryRow(t.Context(), `SELECT g.revoked_at IS NOT NULL,c.state FROM access_grants g
		JOIN access_connections c ON c.organization_id=g.organization_id AND c.id=g.connection_id
		WHERE g.organization_id=$1 AND g.id=$2`, org, item.GrantID).Scan(&grantRevoked, &connectionState)
	if err != nil || !grantRevoked || connectionState != "revoked" {
		t.Fatalf("access chain remained live: revoked=%v connection=%q err=%v", grantRevoked, connectionState, err)
	}
	decisionTx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer decisionTx.Rollback(t.Context())
	_, err = access.PreflightModelInvoke(t.Context(), decisionTx, access.ModelGrant{OrganizationID: org,
		ProjectID: project, GrantID: item.GrantID, GranteeKind: "workload", GranteeID: "blaxsmith-dispatcher",
		Provider: "openai", Model: "gpt-5"})
	if !errors.Is(err, access.ErrDenied) {
		t.Fatalf("revoked grant remained authorized: %v", err)
	}
	var auditEvents int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM identity_audit_events
		WHERE organization_id=$1 AND action='access.project_model.revoked' AND subject_id=$2`, org, item.ID).Scan(&auditEvents); err != nil || auditEvents != 1 {
		t.Fatalf("revocation audit count=%d, err=%v", auditEvents, err)
	}
	if _, err := store.CreateProjectModelAccessAs(t.Context(), owner, project, "openai", "gpt-5", []byte("replacement-secret"), secrets); err != nil {
		t.Fatalf("regrant after revocation: %v", err)
	}
}
