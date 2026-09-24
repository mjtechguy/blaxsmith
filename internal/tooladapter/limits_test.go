package tooladapter

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

func TestIdleTimeoutStopsOnlyASilentStage(t *testing.T) {
	requireTmux(t)
	old := idleTick
	idleTick = 200 * time.Millisecond
	t.Cleanup(func() { idleTick = old })
	profile := recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "high"}

	// Busy for 4s with a 2s idle timeout: each event line is progress.
	busy := fakeHarness(t, "codex", "codex-cli 0.156.1",
		"for i in 1 2 3 4 5 6 7 8; do echo '{\"type\":\"turn.started\"}'; sleep 0.5; done\n")
	in, err := Prepare(busy, profile, "Work", 2*time.Second, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(t.Context(), in, t.TempDir(), []string{"OPENAI_API_KEY=k"}); err != nil {
		t.Fatalf("a stage making progress outlived its idle timeout and failed: %v", err)
	}

	// Silent for longer than the idle timeout: stopped with ErrIdle.
	silent := fakeHarness(t, "codex", "codex-cli 0.156.1", "sleep 30\n")
	in, err = Prepare(silent, profile, "Work", 2*time.Second, 4096)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := Run(t.Context(), in, t.TempDir(), []string{"OPENAI_API_KEY=k"}); !errors.Is(err, ErrIdle) || time.Since(start) > 10*time.Second {
		t.Fatalf("silent stage: %v after %s", err, time.Since(start))
	}

	// max_runtime caps total time even while the stage makes progress.
	in, err = Prepare(busy, profile, "Work", 2*time.Second, 4096)
	if err != nil {
		t.Fatal(err)
	}
	in.maxRuntime = time.Second
	if _, err := Run(t.Context(), in, t.TempDir(), []string{"OPENAI_API_KEY=k"}); err == nil {
		t.Fatal("max runtime did not stop the stage")
	}
}

func TestLeaseWatchHonorsPlatformRenewal(t *testing.T) {
	old := leaseTick
	leaseTick = 50 * time.Millisecond
	t.Cleanup(func() { leaseTick = old })
	path := statePath("lease-expires")
	t.Cleanup(func() { os.Remove(path) })
	if err := os.MkdirAll(StateDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	renew := func(at time.Time) {
		if err := os.WriteFile(path, []byte(strconv.FormatInt(at.Unix(), 10)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	renew(time.Now().Add(-time.Minute)) // a stale value never shortens the lease
	ctx, cancel := watchLease(context.Background(), time.Now().Add(time.Second))
	defer cancel()
	renew(time.Now().Add(3 * time.Second))
	time.Sleep(1500 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("renewed lease expired at the original expiry")
	}
	select {
	case <-ctx.Done():
		if !errors.Is(context.Cause(ctx), ErrLeaseExpired) {
			t.Fatalf("cause: %v", context.Cause(ctx))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease without further renewal never expired")
	}
}
