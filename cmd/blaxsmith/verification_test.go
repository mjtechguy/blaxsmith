package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/terminal/terminaltest"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestCheckProcessUsesDaemonExitAndBoundsOutput(t *testing.T) {
	fake, conn := terminaltest.Start(t)
	router := terminal.NewRouterConn(conn)
	guest, err := router.Guest("team", "attempt-one")
	if err != nil {
		t.Fatal(err)
	}
	fake.Stdout = func([]string) string { return "check output" }
	out, code, err := checkProcess(t.Context(), guest, []string{"test-command", "$(not-a-shell)"}, time.Second)
	if err != nil || code != 0 || string(out) != "check output" {
		t.Fatalf("process observation: %q %d %v", out, code, err)
	}
	commands := fake.Commands("/usr/bin/env")
	if len(commands) != 1 || !slices.Contains(commands[0].Argv, "-i") || commands[0].Argv[len(commands[0].Argv)-1] != "$(not-a-shell)" {
		t.Fatalf("argv: %+v", commands)
	}
	fake.ExitCode = func([]string) int { return 7 }
	if _, code, err := checkProcess(t.Context(), guest, []string{"test-command"}, time.Second); err != nil || code != 7 {
		t.Fatalf("failed exit lost: %d %v", code, err)
	}
	fake.Stdout = func([]string) string { return strings.Repeat("x", (64<<10)+1) }
	if _, code, err := checkProcess(t.Context(), guest, []string{"test-command"}, time.Second); err == nil || code == 0 {
		t.Fatalf("oversized output accepted: %d %v", code, err)
	}
}

func TestCheckProcessKillsOnCancellation(t *testing.T) {
	fake, conn := terminaltest.Start(t)
	fake.Hold = func([]string) bool { return true }
	guest, err := terminal.NewRouterConn(conn).Guest("team", "cancelled-check")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	if _, code, err := checkProcess(ctx, guest, []string{"slow-check"}, time.Minute); err == nil || code == 0 {
		t.Fatalf("cancelled check passed: %d %v", code, err)
	}
	processes := fake.Snapshot()
	if len(processes) != 1 || !processes[0].Killed {
		t.Fatalf("cancelled process left running: %+v", processes)
	}
}

func TestVerificationOwnershipCancellationPostgres(t *testing.T) {
	pool := terminalTestPool(t)
	store, _ := workflow.New(pool)
	ctx := tenant.System(t.Context())
	var org string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations(id,slug,name) VALUES(gen_random_uuid(),'verify-stop','Verify stop') RETURNING id`).Scan(&org); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, org, "verify-stop", "Verify stop")
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "verify-stop", SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.AddTask(ctx, org, run.ID, "verify", run.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, org, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	deadline, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	err = runOwnedVerification(deadline, store, attempt, func(work context.Context) error {
		if err := store.RequestCancel(ctx, org, run.ID); err != nil {
			return err
		}
		<-work.Done()
		return work.Err()
	})
	if !errors.Is(err, context.Canceled) || deadline.Err() != nil {
		t.Fatalf("cancelled run kept its verifier: %v parent=%v", err, deadline.Err())
	}
}
