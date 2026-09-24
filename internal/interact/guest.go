package interact

import (
	"context"
	"errors"
	"io"

	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"google.golang.org/grpc"
)

// Process is a started guest command. Wait returns after Stdout is drained.
type Process struct {
	Stdout io.Reader
	Stdin  io.WriteCloser // nil unless requested
	Wait   func() (int, error)
}

// GuestExec runs a command inside an attempt's AX guest.
type GuestExec interface {
	Start(ctx context.Context, attempt workflow.Attempt, argv []string, stdin bool) (*Process, error)
}

// AteGuest implements GuestExec over the process-wide terminal.Router, the
// one atenet-router connection (BLAXSMITH_GUEST_ROUTER). Never kubeconfig or
// port-forward.
type AteGuest struct{ Router *terminal.Router }

func (g *AteGuest) Start(ctx context.Context, a workflow.Attempt, argv []string, stdin bool) (*Process, error) {
	if len(argv) == 0 {
		return nil, errors.New("guest command required")
	}
	space, actor := axbridge.Name(a) // the AX actor name equals the Task name
	guest, err := g.Router.Guest(space, actor)
	if err != nil {
		return nil, err
	}
	ctx, client := guest.Process(ctx)
	started, err := client.StartProcess(ctx, &ateenv.StartProcessRequest{Command: argv, Stdin: stdin})
	if err != nil {
		return nil, err
	}
	pid := started.GetProcessId()
	stream, err := client.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{ProcessId: pid, Follow: true})
	if err != nil {
		return nil, err
	}
	out, pipe := io.Pipe()
	done := make(chan struct{})
	code, waitErr := -1, error(nil)
	go func() {
		defer close(done)
		for {
			msg, err := stream.Recv()
			if err != nil {
				if !errors.Is(err, io.EOF) {
					waitErr = err
				} else if code < 0 {
					// Follow ended without an exit record; read the final state.
					p, getErr := client.GetProcess(ctx, &ateenv.GetProcessRequest{ProcessId: pid})
					code, waitErr = int(p.GetExitCode()), getErr
				}
				pipe.CloseWithError(waitErr)
				return
			}
			if exit := msg.GetExit(); exit != nil {
				code = int(exit.GetExitCode())
			}
			if data := msg.GetStdout(); len(data) > 0 {
				if _, err := pipe.Write(data); err != nil {
					waitErr = err
					return
				}
			}
		}
	}()
	p := &Process{Stdout: out, Wait: func() (int, error) { <-done; return code, waitErr }}
	if stdin {
		input, err := client.WriteProcessInput(ctx)
		if err != nil {
			return nil, err
		}
		p.Stdin = &guestInput{stream: input, pid: pid}
	}
	return p, nil
}

type guestInput struct {
	stream grpc.ClientStreamingClient[ateenv.WriteProcessInputRequest, ateenv.WriteProcessInputResponse]
	pid    string
}

func (w *guestInput) Write(b []byte) (int, error) {
	if err := w.stream.Send(&ateenv.WriteProcessInputRequest{ProcessId: w.pid, Data: b}); err != nil {
		return 0, err
	}
	return len(b), nil
}

func (w *guestInput) Close() error {
	if err := w.stream.Send(&ateenv.WriteProcessInputRequest{ProcessId: w.pid, Close: true}); err != nil {
		return err
	}
	_, err := w.stream.CloseAndRecv()
	return err
}
