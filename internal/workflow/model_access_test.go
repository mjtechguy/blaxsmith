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
