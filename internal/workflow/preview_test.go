package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

func TestPreviewPins(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		bundle, policy string
		want           error
	}{
		{"", "", nil}, {a, b, nil}, {b, b, ErrPreviewChanged}, {a, a, ErrPreviewChanged},
		{a, "", ErrInvalid}, {"", b, ErrInvalid}, {strings.ToUpper(a), b, ErrInvalid}, {"bad", b, ErrInvalid},
	} {
		if err := CheckPreview(tc.bundle, tc.policy, a, b); !errors.Is(err, tc.want) {
			t.Fatalf("pins %q/%q: %v, want %v", tc.bundle, tc.policy, err, tc.want)
		}
	}
}

func TestPreviewAdmissionPostgres(t *testing.T) {
	pool := testPool(t)
	store, _ := New(pool)
	ctx := tenant.System(t.Context())
	org := organization(t, pool, "preview")
	project, err := store.CreateProject(ctx, org, "preview", "Preview")
	if err != nil {
		t.Fatal(err)
	}
	caller := reviewer(t, pool, org, "owner", "preview-owner")
	source := recipe.Input{Repo: anvilRepo(t), Ref: "HEAD", Recipe: "native.json", RecipeData: anvilRecipe(t), Scope: "."}
	policy := VerificationPolicy{SchemaVersion: "blaxsmith.verification/v1alpha1", Checks: []VerificationCheck{{ID: "tests", Command: []string{"go", "test", "./..."}}}}
	bundle, err := store.PreviewSource(ctx, caller, project, source)
	if err != nil {
		t.Fatal(err)
	}
	policySHA, err := PreviewVerification(bundle, policy)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 0 {
		t.Fatalf("preview created run: %d %v", count, err)
	}
	in := FrozenRunInput{OrganizationID: org, ProjectID: project, LaunchKey: "preview", Source: source, Verification: policy, ExpectedBundleSHA256: bundle.Digest, ExpectedVerificationSHA256: policySHA}
	changedPolicy := in
	changedPolicy.Verification.Checks = []VerificationCheck{{ID: "tests", Command: []string{"false"}, Mode: "advisory"}}
	if _, err := store.CreateFrozenRun(ctx, changedPolicy); !errors.Is(err, ErrPreviewChanged) {
		t.Fatalf("changed policy launched: %v", err)
	}
	changedSource := in
	r := bundle.Recipe
	r.Acceptance = "policy"
	changedSource.Source.RecipeData, _ = json.Marshal(r)
	if _, err := store.CreateFrozenRun(ctx, changedSource); !errors.Is(err, ErrPreviewChanged) {
		t.Fatalf("changed source launched: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workflow_runs WHERE organization_id=$1`, org).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected launch created run: %d %v", count, err)
	}
	run, err := store.CreateFrozenRun(ctx, in)
	if err != nil || run.BundleSHA256 != bundle.Digest || run.VerificationSHA256 != policySHA {
		t.Fatalf("matching preview launch: %+v %v", run, err)
	}
	// The preview and admission use the same graph/check validation.
	r.Stages = r.Stages[:1]
	changedSource.Source.RecipeData, _ = json.Marshal(r)
	changedSource.ExpectedBundleSHA256, changedSource.ExpectedVerificationSHA256 = "", ""
	invalid, err := store.PreviewSource(ctx, caller, project, changedSource.Source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PreviewVerification(invalid, policy); !errors.Is(err, ErrRecipe) {
		t.Fatalf("preview allowed checks without verify: %v", err)
	}
	if _, err := store.CreateFrozenRun(ctx, changedSource); !errors.Is(err, ErrRecipe) {
		t.Fatalf("admission allowed checks without verify: %v", err)
	}
}
