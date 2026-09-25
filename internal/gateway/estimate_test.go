package gateway

import (
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
)

// TestPacingEstimatePostgres: pacing compares a pool's remaining tokens with
// a real per-request estimate, the stage's recent average or else a
// conservative family default, not a single token.
func TestPacingEstimatePostgres(t *testing.T) {
	pool := testPool(t)
	f := newFixture(t, pool)
	ctx := tenant.System(t.Context())
	estimate := func() int64 {
		t.Helper()
		n, err := EstimateTokens(ctx, pool, f.org, f.project, "implement", "anthropic")
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if got := estimate(); got != 48_000 {
		t.Fatalf("default estimate: %d", got)
	}
	grant := Grant{OrganizationID: f.org, ProjectID: f.project, RunID: f.run, AttemptID: f.attempt, TaskID: f.task,
		StageKey: "implement", Harness: "claude-code", Provider: "anthropic", Model: "m"}
	for _, input := range []int64{8_000, 10_000, 12_000} {
		if err := Record(ctx, pool, Event{Grant: grant, RouteID: "r", RouteKind: "anthropic", API: APIAnthropicMessages,
			Status: "ok", HTTPStatus: 200, StartedAt: time.Now(), Usage: Usage{Input: input, Output: 500, Reported: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if got := estimate(); got != 10_500 {
		t.Fatalf("history estimate: %d", got)
	}
	// 20k tokens left: short of the 48k default, enough for a 10.5k stage.
	m := Metric{Limit: 400_000, Remaining: 20_000, ResetAt: time.Now().Add(time.Minute)}
	if !m.exhausted(48_000, time.Now()) || m.exhausted(10_500, time.Now()) {
		t.Fatal("estimate not applied to remaining tokens")
	}
}
