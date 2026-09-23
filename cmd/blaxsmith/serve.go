package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/catalog"
)

// serve is a loopback-only development API for public release metadata. It
// exposes no accounts, projects, credentials, or worker controls.
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
	mux.HandleFunc("GET /api/v1/tools", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		results := make([]catalog.Result, 0, 3)
		for _, name := range []string{"codex", "claude-code", "opencode"} {
			result, err := catalog.Fetch(ctx, client, name, 20)
			if err != nil {
				http.Error(w, "runtime catalog unavailable", http.StatusBadGateway)
				return
			}
			results = append(results, result)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if err := json.NewEncoder(w).Encode(results); err != nil {
			fmt.Fprintln(os.Stderr, "write catalog response:", err)
		}
	})
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
