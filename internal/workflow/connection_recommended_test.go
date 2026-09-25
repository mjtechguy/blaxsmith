package workflow

import (
	"errors"
	"slices"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestRecommendedModelsArePinnedByManagers(t *testing.T) {
	pool := testPool(t)
	ctx := tenant.System(t.Context())
	store, err := New(pool)
	if err != nil {
		t.Fatal(err)
	}
	org := organization(t, pool, "recommended")
	owner := reviewer(t, pool, org, "owner", "recommended-owner")
	member := reviewer(t, pool, org, "member", "recommended-member")
	secrets := connectionSecrets(t, store)
	c, err := store.CreateAPIKeyConnectionAs(ctx, owner, ScopeOrganization, "", "openai", "", "", []byte("sk-rec"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecommendedModelsAs(ctx, member, c.ID, []string{"gpt-5"}); !errors.Is(err, ErrConnectionDenied) {
		t.Fatalf("member pinned on an organization connection: %v", err)
	}
	if _, err := store.SetRecommendedModelsAs(ctx, owner, c.ID, []string{"gpt-5", "gpt-5"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("duplicate pins accepted: %v", err)
	}
	if got, err := store.SetRecommendedModelsAs(ctx, owner, c.ID, []string{"gpt-5"}); err != nil || !slices.Equal(got, []string{"gpt-5"}) {
		t.Fatalf("pin %v %v", got, err)
	}
	models, _, _, err := store.ListConnectionModelsAs(ctx, owner, c.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		if m.Recommended != (m.ID == "gpt-5") {
			t.Fatalf("recommended flag on %s = %v", m.ID, m.Recommended)
		}
	}
	personal, err := store.CreateAPIKeyConnectionAs(ctx, member, ScopePersonal, "", "openai", "", "", []byte("sk-mine"), testModels, "", secrets)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetRecommendedModelsAs(ctx, member, personal.ID, []string{"text-embedding-3"}); err != nil {
		t.Fatalf("owner pinned own personal connection: %v", err)
	}
	if _, err := store.SetRecommendedModelsAs(ctx, owner, personal.ID, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("org owner changed someone's personal pins: %v", err)
	}
	if _, err := store.SetRecommendedModelsAs(ctx, owner, c.ID, nil); err != nil {
		t.Fatalf("clear pins: %v", err)
	}
	if got, _ := store.recommendedModels(ctx, org, c.ID); len(got) != 0 {
		t.Fatalf("pins not cleared: %v", got)
	}
}
