package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/catalog"
	"golang.org/x/sync/singleflight"
)

const catalogFreshFor = 15 * time.Minute
const catalogStaleFor = 24 * time.Hour
const catalogRetryFor = time.Minute

// ponytail: this preview cache is per process; durable approved runtimes get
// their own catalog store when installation and promotion are implemented.
type catalogService struct {
	client      *http.Client
	mu          sync.RWMutex
	value       *api.ListToolsResponse
	freshUntil  time.Time
	staleUntil  time.Time
	retryAfter  time.Time
	refreshOnce singleflight.Group
}

func (s *catalogService) snapshot(now time.Time) (*api.ListToolsResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.value != nil && now.Before(s.freshUntil) {
		return s.value, true
	}
	if now.Before(s.retryAfter) {
		if s.value != nil && now.Before(s.staleUntil) {
			return &api.ListToolsResponse{Tools: s.value.Tools, Stale: true}, true
		}
		return nil, true
	}
	return nil, false
}

func (s *catalogService) ListTools(ctx context.Context, _ *connect.Request[api.ListToolsRequest]) (*connect.Response[api.ListToolsResponse], error) {
	if current, ready := s.snapshot(time.Now()); ready {
		if current == nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("runtime catalog unavailable"))
		}
		return connect.NewResponse(current), nil
	}
	ch := s.refreshOnce.DoChan("tools", func() (any, error) {
		if current, ready := s.snapshot(time.Now()); ready {
			if current == nil {
				return nil, errors.New("runtime catalog unavailable")
			}
			return current, nil
		}
		refreshCtx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		latest, err := s.fetch(refreshCtx)
		now := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			s.retryAfter = now.Add(catalogRetryFor)
			if s.value != nil && now.Before(s.staleUntil) {
				return &api.ListToolsResponse{Tools: s.value.Tools, Stale: true}, nil
			}
			return nil, err
		}
		s.value = latest
		s.freshUntil = now.Add(catalogFreshFor)
		s.staleUntil = now.Add(catalogStaleFor)
		s.retryAfter = time.Time{}
		return latest, nil
	})
	select {
	case <-ctx.Done():
		return nil, connect.NewError(connect.CodeCanceled, ctx.Err())
	case result := <-ch:
		if result.Err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, errors.New("runtime catalog unavailable"))
		}
		return connect.NewResponse(result.Val.(*api.ListToolsResponse)), nil
	}
}

func (s *catalogService) fetch(ctx context.Context) (*api.ListToolsResponse, error) {
	response := &api.ListToolsResponse{Tools: make([]*api.ToolCatalog, 0, 3)}
	for _, name := range []string{"codex", "claude-code", "opencode"} {
		result, err := catalog.Fetch(ctx, s.client, name, 20)
		if err != nil {
			return nil, err
		}
		entry := &api.ToolCatalog{Tool: result.Tool, Package: result.Package, Source: result.Source,
			FetchedAt: result.FetchedAt.Format(time.RFC3339Nano), LatestStable: result.LatestStable,
			PublisherLatest: result.PublisherLatest, PublisherStable: result.PublisherStable}
		for _, release := range result.Releases {
			entry.Releases = append(entry.Releases, &api.ToolRelease{Version: release.Version,
				PublishedAt: release.PublishedAt.Format(time.RFC3339Nano), Integrity: release.Integrity,
				Tarball: release.Tarball})
		}
		response.Tools = append(response.Tools, entry)
	}
	return response, nil
}

// serve is a loopback-only development API for public release metadata.
func serve(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	port := flags.Int("port", 8001, "loopback development API port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *port < 1 || *port > 65535 {
		return fmt.Errorf("serve requires a valid --port and no positional arguments")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	mux := http.NewServeMux()
	path, handler := apiv1connect.NewCatalogServiceHandler(&catalogService{client: client})
	mux.Handle("/api"+path, http.StripPrefix("/api", handler))
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *port))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	fmt.Fprintf(os.Stderr, "Blaxsmith development API on http://%s\n", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
