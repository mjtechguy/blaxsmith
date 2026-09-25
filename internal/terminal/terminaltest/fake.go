// Package terminaltest provides an in-process fake of the Substrate guest
// ProcessService for terminal gateway tests.
package terminaltest

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	ateenv "github.com/agent-substrate/env/proto/ateenv/v1alpha"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// Fake keeps `script` attach commands running until killed or Exit; every
// other command exits 0 with Stdout(argv).
type Fake struct {
	ateenv.UnimplementedProcessServiceServer
	ateenv.UnimplementedFileSystemServiceServer
	mu      sync.Mutex
	seq     int
	procs   map[string]*proc
	order   []*proc
	targets []string
	Stdout  func(argv []string) string
}

type proc struct {
	argv   []string
	auth   string
	stdin  []byte
	killed bool
	exit   chan int
	out    chan []byte
}

// Proc is a snapshot of one started process.
type Proc struct {
	ID     string
	Argv   []string
	Stdin  []byte
	Killed bool
	Target string // ate-target-actor metadata
	Auth   string // authorization metadata
}

// Start serves the fake over bufconn and returns a client connection.
func Start(t testing.TB) (*Fake, *grpc.ClientConn) {
	t.Helper()
	fake, dialer := Serve(t)
	conn, err := grpc.NewClient("passthrough:///guest", grpc.WithTransportCredentials(insecure.NewCredentials()), dialer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return fake, conn
}

// Serve serves the fake over bufconn with opts (e.g. grpc.Creds) and returns
// the dial option that reaches it.
func Serve(t testing.TB, opts ...grpc.ServerOption) (*Fake, grpc.DialOption) {
	t.Helper()
	fake := &Fake{procs: map[string]*proc{}}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer(opts...)
	ateenv.RegisterProcessServiceServer(server, fake)
	ateenv.RegisterFileSystemServiceServer(server, fake)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return fake, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) })
}

func (f *Fake) StartProcess(ctx context.Context, req *ateenv.StartProcessRequest) (*ateenv.Process, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	id := fmt.Sprint(f.seq)
	p := &proc{argv: req.Command, auth: strings.Join(md.Get("authorization"), ","), exit: make(chan int, 1), out: make(chan []byte, 4)}
	if req.Command[0] == "script" {
		p.out <- []byte("screen\r\n")
	} else {
		if f.Stdout != nil {
			p.out <- []byte(f.Stdout(req.Command))
		}
		p.exit <- 0
	}
	f.procs[id] = p
	f.order = append(f.order, p)
	f.targets = append(f.targets, strings.Join(md.Get("ate-target-actor"), ","))
	return &ateenv.Process{ProcessId: id}, nil
}

func (f *Fake) get(id string) *proc {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[id]
}

// Output writes data to a running attach process's stdout stream.
func (f *Fake) Output(id string, data []byte) { f.get(id).out <- data }

// Exit ends a running attach process.
func (f *Fake) Exit(id string, code int) { f.get(id).exit <- code }

func (f *Fake) StreamProcessOutput(req *ateenv.StreamProcessOutputRequest, s grpc.ServerStreamingServer[ateenv.ProcessOutput]) error {
	p := f.get(req.ProcessId)
	for {
		select {
		case b := <-p.out:
			if err := s.Send(&ateenv.ProcessOutput{Output: &ateenv.ProcessOutput_Stdout{Stdout: b}}); err != nil {
				return err
			}
		case code := <-p.exit:
			for len(p.out) > 0 {
				_ = s.Send(&ateenv.ProcessOutput{Output: &ateenv.ProcessOutput_Stdout{Stdout: <-p.out}})
			}
			return s.Send(&ateenv.ProcessOutput{Output: &ateenv.ProcessOutput_Exit{Exit: &ateenv.Process{ExitCode: int32(code)}}})
		case <-s.Context().Done():
			return nil
		}
	}
}

func (f *Fake) WriteProcessInput(s grpc.ClientStreamingServer[ateenv.WriteProcessInputRequest, ateenv.WriteProcessInputResponse]) error {
	var p *proc
	for {
		msg, err := s.Recv()
		if err != nil {
			return nil
		}
		if p == nil {
			p = f.get(msg.ProcessId)
		}
		f.mu.Lock()
		p.stdin = append(p.stdin, msg.Data...)
		f.mu.Unlock()
	}
}

func (f *Fake) SignalProcess(_ context.Context, req *ateenv.SignalProcessRequest) (*ateenv.Process, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[req.ProcessId].killed = true
	return &ateenv.Process{ProcessId: req.ProcessId}, nil
}

func (f *Fake) ReadFile(req *ateenv.ReadFileRequest, s grpc.ServerStreamingServer[ateenv.ReadFileResponse]) error {
	return s.Send(&ateenv.ReadFileResponse{Chunk: []byte("file:" + req.Path)})
}

// Snapshot returns every process started so far, in order.
func (f *Fake) Snapshot() []Proc {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Proc, len(f.order))
	for i, p := range f.order {
		out[i] = Proc{ID: fmt.Sprint(i + 1), Argv: p.argv, Stdin: slices.Clone(p.stdin), Killed: p.killed, Target: f.targets[i], Auth: p.auth}
	}
	return out
}

// Commands returns processes whose joined argv starts with prefix.
func (f *Fake) Commands(prefix string) []Proc {
	var out []Proc
	for _, p := range f.Snapshot() {
		if strings.HasPrefix(strings.Join(p.Argv, " "), prefix) {
			out = append(out, p)
		}
	}
	return out
}
