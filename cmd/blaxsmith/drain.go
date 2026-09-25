package main

import (
	"context"
	"net/http"
	"sync"
	"time"
)

// drainer is the app's shutdown signal. Once it begins, readiness fails so no
// new traffic is routed here, and long-lived streams (run activity SSE and
// terminal sockets, which http.Server.Shutdown would otherwise wait on or not
// track at all) end with a "going away" hint so clients reconnect promptly to
// a healthy replica. Unary RPCs in flight are left to http.Server.Shutdown.
type drainer struct {
	once    sync.Once
	closing chan struct{}
}

func newDrainer() *drainer { return &drainer{closing: make(chan struct{})} }

func (d *drainer) begin() {
	if d != nil {
		d.once.Do(func() { close(d.closing) })
	}
}

// done is nil (never ready) for a nil drainer, as in handler tests.
func (d *drainer) done() <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.closing
}

func (d *drainer) draining() bool {
	select {
	case <-d.done():
		return true
	default:
		return false
	}
}

// shutdown drains server: readiness fails and streams are sent away first,
// then the listener closes and in-flight requests get up to timeout.
func (d *drainer) shutdown(ctx context.Context, server *http.Server, timeout time.Duration) error {
	d.begin()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	return server.Shutdown(ctx)
}
