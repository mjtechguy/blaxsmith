// Package terminal bridges a browser terminal to the tmux session an AX
// attempt runs its harness in, through the public Substrate guest API.
// Terminal bytes are relayed only; nothing here persists a transcript,
// because raw harness output can contain secrets.
package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/durationpb"
)

// Guest-side contract (docs/interactive-sessions.md).
const (
	TmuxSocket  = "/tmp/blaxsmith/tmux.sock"
	TmuxSession = "agent"
	ToolWorker  = "/usr/local/bin/blaxsmith-tool-worker"
)

const (
	execTimeout   = 30 * time.Second
	attachTimeout = 8 * time.Hour // guest kills a leaked attach client after this
	maxExecOutput = 64 << 10
)

var (
	ErrGuest    = errors.New("guest command failed")
	actorName   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	errTooLarge = errors.New("guest output exceeds limit")
)

// Router is one process-wide gRPC connection to the in-cluster Substrate
// atenet-router. Each call names its guest with ate-target-actor metadata.
//
// ponytail: guest calls need AX Task spec.debug=true, which exposes the whole
// guest ProcessService/FileSystemService to anything that can reach the router.
// Blaxsmith (the connector) must be the only permitted caller, enforced by a
// NetworkPolicy on atenet-router; replace this with a narrow AX-owned terminal
// API (attach/resize/takeover only, authenticated per attempt) before
// multi-tenant release. The router hop is plaintext h2c inside the cluster.
type Router struct {
	conn    *grpc.ClientConn
	process ateenv.ProcessServiceClient
	files   ateenv.FileSystemServiceClient
}

// NewRouter dials host:port lazily. It never loads a kubeconfig or port-forward.
func NewRouter(address string) (*Router, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || host == "" || port == "" || strings.Contains(address, "/") {
		return nil, errors.New("guest router must be host:port")
	}
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, fmt.Errorf("guest router: %w", err)
	}
	r := NewRouterConn(conn)
	r.conn = conn
	return r, nil
}

// NewRouterConn wraps an existing connection (tests use bufconn).
func NewRouterConn(cc grpc.ClientConnInterface) *Router {
	return &Router{process: ateenv.NewProcessServiceClient(cc), files: ateenv.NewFileSystemServiceClient(cc)}
}

func (r *Router) Close() error {
	if r == nil || r.conn == nil {
		return nil
	}
	return r.conn.Close()
}

// Guest addresses one actor. atespace and actor come from the attempt's pinned
// AX runtime readback, never from the browser.
func (r *Router) Guest(atespace, actor string) (*Guest, error) {
	if r == nil || !actorName.MatchString(atespace) || !actorName.MatchString(actor) {
		return nil, errors.New("invalid guest target")
	}
	return &Guest{router: r, target: atespace + "/" + actor}, nil
}

type Guest struct {
	router *Router
	target string
}

func (g *Guest) ctx(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "ate-target-actor", g.target)
}

// Process returns the guest ProcessService client and ctx addressed to this
// guest (interact.AteGuest runs `bx` commands over it).
func (g *Guest) Process(ctx context.Context) (context.Context, ateenv.ProcessServiceClient) {
	return g.ctx(ctx), g.router.process
}

// Exec runs argv to completion without stdin and returns its exit code and at
// most max bytes of stdout (more is an error).
func (g *Guest) Exec(ctx context.Context, argv []string, max int) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(g.ctx(ctx), execTimeout+5*time.Second)
	defer cancel()
	proc, err := g.router.process.StartProcess(ctx, &ateenv.StartProcessRequest{Command: argv,
		Timeout: durationpb.New(execTimeout)})
	if err != nil {
		return -1, nil, fmt.Errorf("%w: start: %v", ErrGuest, err)
	}
	stream, err := g.router.process.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{
		ProcessId: proc.GetProcessId(), Follow: true})
	if err != nil {
		return -1, nil, fmt.Errorf("%w: stream: %v", ErrGuest, err)
	}
	var out []byte
	for {
		msg, err := stream.Recv()
		if err != nil {
			return -1, nil, fmt.Errorf("%w: output ended without exit: %v", ErrGuest, err)
		}
		if exit := msg.GetExit(); exit != nil {
			return int(exit.GetExitCode()), out, nil
		}
		if len(out)+len(msg.GetStdout()) > max {
			return -1, nil, errTooLarge
		}
		out = append(out, msg.GetStdout()...)
	}
}

func (g *Guest) tmux(ctx context.Context, args ...string) ([]byte, error) {
	code, out, err := g.Exec(ctx, append([]string{"tmux", "-S", TmuxSocket}, args...), maxExecOutput)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, fmt.Errorf("%w: tmux %s exited %d", ErrGuest, args[0], code)
	}
	return out, nil
}

// Resize sets the shared window size. Callers must allow only the controller.
func (g *Guest) Resize(ctx context.Context, cols, rows int) error {
	if cols < 10 || cols > 1000 || rows < 5 || rows > 500 {
		return errors.New("invalid terminal size")
	}
	_, err := g.tmux(ctx, "resize-window", "-t", TmuxSession, "-x", strconv.Itoa(cols), "-y", strconv.Itoa(rows))
	return err
}

// TakeOver interrupts the autonomous harness and respawns the pane with the
// harness's native interactive resume. The resume argv is read first so a
// failure leaves the agent running untouched.
func (g *Guest) TakeOver(ctx context.Context) error {
	code, out, err := g.Exec(ctx, []string{ToolWorker, "resume-argv"}, maxExecOutput)
	if err != nil {
		return err
	}
	var argv []string
	if code != 0 || json.Unmarshal(out, &argv) != nil || len(argv) == 0 || len(argv) > 256 {
		return fmt.Errorf("%w: resume-argv unavailable", ErrGuest)
	}
	for _, arg := range argv {
		if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
			return fmt.Errorf("%w: resume-argv invalid", ErrGuest)
		}
	}
	if _, err := g.tmux(ctx, "send-keys", "-t", TmuxSession, "C-c"); err != nil {
		return err
	}
	// Bounded wait for the harness to exit; respawn-pane -k kills a straggler.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		dead, err := g.tmux(ctx, "display-message", "-p", "-t", TmuxSession, "#{pane_dead}")
		if err != nil {
			return err
		}
		if strings.TrimSpace(string(dead)) == "1" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	respawn := append([]string{"respawn-pane", "-k", "-t", TmuxSession, "--", ToolWorker, "pane", "--interactive", "--"}, argv...)
	_, err = g.tmux(ctx, respawn...)
	return err
}

// handBackPause separates the typed /exit from its Enter: Codex swallows an
// Enter that arrives straight after typed text.
var handBackPause = time.Second

// HandBack asks the native TUI to exit; `pane --interactive` then signals
// blaxsmith-done exactly like the autonomous path. All three pinned harnesses
// (Claude Code, Codex, OpenCode) accept /exit.
func (g *Guest) HandBack(ctx context.Context) error {
	if _, err := g.tmux(ctx, "send-keys", "-t", TmuxSession, "-l", "/exit"); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(handBackPause):
	}
	_, err := g.tmux(ctx, "send-keys", "-t", TmuxSession, "Enter")
	return err
}

// ReadFile returns a guest file of at most max bytes.
func (g *Guest) ReadFile(ctx context.Context, path string, max int) ([]byte, error) {
	var out bytes.Buffer
	if err := g.readFile(ctx, path, &out, int64(max), execTimeout); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// WriteFile replaces a small guest file (the lease-expires renewal notice).
func (g *Guest) WriteFile(ctx context.Context, path string, data []byte, mode uint32) error {
	ctx, cancel := context.WithTimeout(g.ctx(ctx), execTimeout)
	defer cancel()
	stream, err := g.router.files.WriteFile(ctx)
	if err == nil {
		err = stream.Send(&ateenv.WriteFileRequest{Path: path, Mode: mode, Chunk: data})
	}
	if err == nil {
		_, err = stream.CloseAndRecv()
	}
	if err != nil {
		return fmt.Errorf("%w: write: %w", ErrGuest, err)
	}
	return nil
}

// ReadFileTo streams a guest file of at most max bytes into w (large files,
// such as the stage's result bundle, never sit in memory).
func (g *Guest) ReadFileTo(ctx context.Context, path string, w io.Writer, max int64) error {
	return g.readFile(ctx, path, w, max, 5*time.Minute)
}

func (g *Guest) readFile(ctx context.Context, path string, w io.Writer, max int64, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(g.ctx(ctx), timeout)
	defer cancel()
	stream, err := g.router.files.ReadFile(ctx, &ateenv.ReadFileRequest{Path: path})
	if err != nil {
		return fmt.Errorf("%w: read: %w", ErrGuest, err)
	}
	var n int64
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: read: %w", ErrGuest, err)
		}
		if n += int64(len(msg.GetChunk())); n > max {
			return errTooLarge
		}
		if _, err := w.Write(msg.GetChunk()); err != nil {
			return err
		}
	}
}

// Attachment is one tmux client inside a guest pty (`script` provides it,
// since the guest API has none). View attachments use tmux -r, which is also
// ignore-size, so viewers never resize the agent's window.
type Attachment struct {
	guest  *Guest
	id     string
	cancel context.CancelFunc
	in     grpc.ClientStreamingClient[ateenv.WriteProcessInputRequest, ateenv.WriteProcessInputResponse]
	out    grpc.ServerStreamingClient[ateenv.ProcessOutput]
}

func AttachCommand(control bool) []string {
	attach := "tmux -S " + TmuxSocket + " attach -t " + TmuxSession
	if !control {
		attach += " -r"
	}
	return []string{"script", "-qfc", attach, "/dev/null"}
}

func (g *Guest) Attach(ctx context.Context, control bool) (*Attachment, error) {
	ctx, cancel := context.WithCancel(g.ctx(ctx))
	proc, err := g.router.process.StartProcess(ctx, &ateenv.StartProcessRequest{Command: AttachCommand(control),
		Stdin: true, Env: map[string]string{"TERM": "xterm-256color"}, Timeout: durationpb.New(attachTimeout)})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("%w: attach: %v", ErrGuest, err)
	}
	a := &Attachment{guest: g, id: proc.GetProcessId(), cancel: cancel}
	if a.out, err = g.router.process.StreamProcessOutput(ctx, &ateenv.StreamProcessOutputRequest{
		ProcessId: a.id, Follow: true}); err == nil {
		a.in, err = g.router.process.WriteProcessInput(ctx)
	}
	if err == nil {
		// The first message names the process; it carries no input.
		err = a.in.Send(&ateenv.WriteProcessInputRequest{ProcessId: a.id})
	}
	if err != nil {
		a.Close()
		return nil, fmt.Errorf("%w: attach streams: %v", ErrGuest, err)
	}
	return a, nil
}

// Write forwards controller keystrokes. Callers enforce control.
func (a *Attachment) Write(p []byte) error {
	return a.in.Send(&ateenv.WriteProcessInputRequest{Data: p})
}

// Recv returns the next output chunk, or exit != nil once the client exited.
func (a *Attachment) Recv() (data []byte, exit *int, err error) {
	for {
		msg, err := a.out.Recv()
		if err != nil {
			return nil, nil, err
		}
		if e := msg.GetExit(); e != nil {
			code := int(e.GetExitCode())
			return nil, &code, nil
		}
		if b := append(msg.GetStdout(), msg.GetStderr()...); len(b) > 0 {
			return b, nil, nil
		}
	}
}

// Close ends the streams and kills the guest-side client so no tmux client
// outlives the browser connection. Idempotent.
func (a *Attachment) Close() {
	a.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = a.guest.router.process.SignalProcess(a.guest.ctx(ctx), &ateenv.SignalProcessRequest{
		ProcessId: a.id, Signal: ateenv.Signal_SIGNAL_KILL})
}
