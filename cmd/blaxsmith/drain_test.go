package main

import (
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestDrainerGracefulShutdown(t *testing.T) {
	drain := newDrainer()
	started := make(chan struct{})
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if drain.draining() {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	// An in-flight unary RPC that is still working when SIGTERM arrives.
	mux.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	// A long-lived stream that only the drain signal ends.
	mux.HandleFunc("/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-drain.done():
			_, _ = io.WriteString(w, "going away")
		case <-r.Context().Done():
		}
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	base := "http://" + listener.Addr().String()
	client := &http.Client{Timeout: 5 * time.Second}

	if response, err := client.Get(base + "/healthz"); err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("ready before drain: %v %v", response, err)
	}
	stream, err := client.Get(base + "/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	slow := make(chan string, 1)
	go func() {
		response, err := client.Get(base + "/slow")
		if err != nil {
			slow <- err.Error()
			return
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		slow <- string(body)
	}()
	<-started

	stopped := make(chan error, 1)
	go func() { stopped <- drain.shutdown(t.Context(), server, 5*time.Second) }()
	if body, err := io.ReadAll(stream.Body); err != nil || string(body) != "going away" {
		t.Fatalf("stream was not sent away on drain: %q %v", body, err)
	}
	if !drain.draining() {
		t.Fatal("readiness still passes while draining")
	}
	// New connections are refused once the listener closes.
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", listener.Addr().String(), 200*time.Millisecond)
		if err != nil {
			break
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("listener still accepts connections while draining")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-stopped:
		t.Fatalf("shutdown returned before the in-flight request finished: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if got := <-slow; got != "finished" {
		t.Fatalf("in-flight request was cut off: %q", got)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
}
