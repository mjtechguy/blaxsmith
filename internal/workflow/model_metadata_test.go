package workflow

import (
	"slices"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

// The stored list keeps what the provider reported; the manifest overlays
// legacy/default/efforts at read time and the harness filter narrows efforts.
func TestConnectionModelMetadataEffortFiltering(t *testing.T) {
	pool := testPool(t)
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "model-meta")
	owner := reviewer(t, pool, org, "owner", "model-meta-owner")
	secrets := connectionSecrets(t, store)
	models := []access.CatalogModel{
		{ID: "claude-opus-5", DisplayName: "Claude Opus 5"},
		{ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6"},
		{ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", Meta: access.ModelMeta{Efforts: []string{"low", "high", "bogus effort"}}},
		{ID: "claude-mystery-1", DisplayName: "Mystery"},
	}
	c, err := store.CreateAPIKeyConnectionAs(t.Context(), owner, ScopePersonal, "", "anthropic", "", []byte("sk-ant-meta"), models, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	list, _, _, err := store.ListConnectionModelsAs(t.Context(), owner, c.ID, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ConnectionModel{}
	for _, m := range list {
		byID[m.ID] = m
	}
	if m := byID["claude-opus-5"]; !m.IsDefault || m.Legacy || m.DefaultEffort != "high" || !slices.Equal(m.Efforts, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("opus 5: %+v", m.ModelMeta)
	}
	if m := byID["claude-opus-4-6"]; !m.Legacy || slices.Contains(m.Efforts, "xhigh") {
		t.Fatalf("opus 4.6: %+v", m.ModelMeta)
	}
	// Reported efforts were stored (the malformed one dropped) and win over
	// the manifest; the manifest default "high" survives because it is listed.
	if m := byID["claude-sonnet-5"]; !slices.Equal(m.Efforts, []string{"low", "high"}) || m.DefaultEffort != "high" {
		t.Fatalf("sonnet 5: %+v", m.ModelMeta)
	}
	if m := byID["claude-mystery-1"]; len(m.Efforts) != 0 || m.DefaultEffort != "" {
		t.Fatalf("unknown model: %+v", m.ModelMeta)
	}
	var stored []string
	if err := pool.QueryRow(t.Context(), `SELECT efforts FROM access_connection_models WHERE connection_id=$1 AND model_id='claude-sonnet-5'`,
		c.ID).Scan(&stored); err != nil || !slices.Equal(stored, []string{"low", "high"}) {
		t.Fatalf("stored efforts %v %v", stored, err)
	}
}
