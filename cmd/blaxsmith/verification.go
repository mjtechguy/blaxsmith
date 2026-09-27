package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"google.golang.org/protobuf/types/known/durationpb"
)

// checkProcess reads the guest daemon's exit observation, never a file or a
// model verdict. The guest daemon enforces a process-group timeout.
func checkProcess(ctx context.Context, guest *terminal.Guest, argv []string, timeout time.Duration) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout+5*time.Second)
	defer cancel()
	ctx, client := guest.Process(ctx)
	// env -i removes ambient credentials/config; no shell interprets argv.
	command := append([]string{"/usr/bin/env", "-i", "PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "LANG=C.UTF-8"}, argv...)
	p, err := client.StartProcess(ctx, &ateenv.StartProcessRequest{Command: command, Cwd: "/workspace/source", Timeout: durationpb.New(timeout)})
	if err != nil {
		return nil, -1, err
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		_, _ = client.SignalProcess(stop, &ateenv.SignalProcessRequest{ProcessId: p.GetProcessId(), Signal: 9})
	}()
	stream, err := client.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{ProcessId: p.GetProcessId(), Follow: true})
	if err != nil {
		return nil, -1, err
	}
	var output []byte
	for {
		m, err := stream.Recv()
		if err != nil {
			return output, -1, err
		}
		if e := m.GetExit(); e != nil {
			return output, int(e.GetExitCode()), nil
		}
		for _, b := range [][]byte{m.GetStdout(), m.GetStderr()} {
			if len(output)+len(b) > 64<<10 {
				return output, -1, errors.New("check output exceeded 64 KiB")
			}
			output = append(output, b...)
		}
	}
}

func independentVerifier(store *workflow.Store, router *terminal.Router) func(context.Context, workflow.Attempt, *axbridge.Bridge) (bool, error) {
	return func(ctx context.Context, a workflow.Attempt, bridge *axbridge.Bridge) (bool, error) {
		f, err := store.LoadFrozenTask(ctx, a.OrganizationID, a.RunID, a.TaskID)
		if err != nil {
			return false, err
		}
		if f.Stage.Kind != "verify" {
			return false, nil
		}
		err = runOwnedVerification(ctx, store, a, func(ctx context.Context) error {
			return store.WithAttemptDispatchLock(ctx, a, func(ctx context.Context) error {
				recorded, err := store.HasAttemptResult(ctx, a)
				if err != nil || recorded {
					return err
				}
				claimed, err := store.ClaimVerification(ctx, a)
				if err != nil {
					return err
				}
				if !claimed {
					return nil
				} // stop without a result: retry in a fresh actor
				binding, err := store.GetRuntimeBinding(ctx, a)
				if err != nil {
					return err
				}
				runtime, err := bridge.Actor.Current(ctx, binding.AXAtespace, binding.AXTask)
				if err != nil {
					return err
				}
				if runtime.Actor.UID != binding.ActorUID || runtime.TemplateUID != binding.TemplateUID || runtime.Image != binding.Image {
					return axbridge.ErrMismatch
				}
				guest, err := router.Guest(binding.AXAtespace, binding.AXTask)
				if err != nil {
					return err
				}
				input, err := store.LoadInputCommit(ctx, a)
				if err != nil {
					return err
				}
				run, err := store.GetRun(ctx, a.OrganizationID, a.RunID)
				if err != nil {
					return err
				}
				if _, code, err := checkProcess(ctx, guest, []string{terminal.ToolWorker, "verify-prepare"}, 30*time.Second); err != nil || code != 0 {
					return errors.New("could not prepare an isolated verification checkout")
				}
				outputs := [][]byte{}
				observations := []workflow.CheckObservation{}
				// No command has run in this sandbox yet: the runner still waits behind
				// its model gate. A crash above leaves verification_started set and forces
				// a new actor rather than rerunning in this sandbox.
				for i, c := range f.Verification.Checks {
					if c.Mode == "off" {
						continue
					}
					observation := workflow.CheckObservation{Check: c.ID, ExitCode: -1, Verdict: "blocked"}
					var output []byte
					head, code, err := checkProcess(ctx, guest, []string{"/usr/bin/git", "rev-parse", "HEAD"}, 10*time.Second)
					if err != nil || code != 0 || strings.TrimSpace(string(head)) != input {
						observation.Summary = "Candidate checkout does not match the frozen revision."
					} else {
						// Protected test scripts/configuration must be unchanged from the
						// trusted source revision. Missing baseline objects fail closed.
						protected := true
						for _, p := range c.TrustedPaths {
							_, code, err = checkProcess(ctx, guest, []string{"/usr/bin/git", "cat-file", "-e", run.SourceCommit + ":" + p}, 10*time.Second)
							if err == nil && code == 0 {
								_, code, err = checkProcess(ctx, guest, []string{"/usr/bin/git", "diff", "--no-ext-diff", "--no-textconv", "--quiet", run.SourceCommit, "--", p}, 10*time.Second)
							}
							if err != nil || code != 0 {
								protected = false
								break
							}
						}
						if !protected {
							observation.Summary = "A protected check file changed or is unavailable."
						} else {
							limit := min(time.Duration(f.Bundle.Recipe.Limits.TimeoutSeconds)*time.Second, 2*time.Minute)
							if limit <= 0 {
								limit = time.Minute
							}
							argv := append([]string{"/usr/bin/setpriv", fmt.Sprintf("--reuid=%d", 60000+i), fmt.Sprintf("--regid=%d", 60000+i), "--clear-groups", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs",
								"/usr/bin/env", fmt.Sprintf("HOME=/tmp/blaxsmith-verification/%d", i), fmt.Sprintf("TMPDIR=/tmp/blaxsmith-verification/%d", i), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=/workspace/source"}, c.Command...)
							output, code, err = checkProcess(ctx, guest, argv, limit)
							observation.ExitCode = code
							if err != nil {
								observation.Summary = "Check could not complete: " + err.Error()
							} else if code != 0 {
								observation.Verdict = "fail"
								observation.Summary = fmt.Sprintf("Command exited %d.", code)
							} else {
								// A check that mutates the candidate cannot attest that revision.
								_, dirtCode, dirtErr := checkProcess(ctx, guest, []string{"/usr/bin/git", "diff", "--no-ext-diff", "--no-textconv", "--exit-code", input, "--"}, 10*time.Second)
								if dirtErr != nil || dirtCode != 0 {
									observation.Summary = "Check modified tracked candidate files."
								} else {
									observation.Verdict = "pass"
									observation.Summary = "Frozen command completed on the candidate revision."
								}
							}
						}
					}
					outputs = append(outputs, output)
					observations = append(observations, observation)
					if ctx.Err() != nil {
						return ctx.Err()
					}
				}
				current, err := bridge.Actor.Current(ctx, binding.AXAtespace, binding.AXTask)
				if err != nil {
					return err
				}
				if current.Actor.UID != binding.ActorUID || current.TemplateUID != binding.TemplateUID || current.Image != binding.Image {
					return axbridge.ErrMismatch
				}
				return store.RecordVerification(ctx, a, input, observations, outputs)
			})
		})
		return true, err
	}
}

// Losing run/attempt ownership interrupts an in-flight command, not just the
// next evidence write. checkProcess attempts process cleanup before this returns;
// actor-gone proof is still required by completion.
func runOwnedVerification(ctx context.Context, store *workflow.Store, a workflow.Attempt, work func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			probe, stop := context.WithTimeout(ctx, 3*time.Second)
			run, state, sealed, err := store.CurrentAttempt(probe, a)
			stop()
			if err != nil || run != "active" || state != "running" || !sealed {
				cancel()
				return
			}
			if !waitContext(ctx, 2*time.Second) {
				return
			}
		}
	}()
	defer func() { cancel(); <-done }()
	return work(ctx)
}
