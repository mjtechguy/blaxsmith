package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatchMaintenanceSurvivesSlowWorkAndJoinsOnShutdown(t *testing.T) {
	for _, cause := range []string{"leadership-lost", "shutdown"} {
		t.Run(cause, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			workStarted, leaseStarted, cancelStarted := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var stopped atomic.Int32
			maintenance := func(started chan struct{}) func(context.Context) {
				return func(ctx context.Context) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > 30*time.Second {
						t.Error("maintenance has no bounded deadline")
					}
					close(started)
					<-ctx.Done()
					stopped.Add(1)
				}
			}
			superviseDispatch(ctx, func(probe context.Context) error {
				deadline, ok := probe.Deadline()
				if !ok || time.Until(deadline) > 3*time.Second {
					t.Error("heartbeat has no bounded deadline")
				}
				for _, ready := range []chan struct{}{workStarted, leaseStarted, cancelStarted} {
					select {
					case <-ready:
					case <-probe.Done():
						t.Error("slow work starved maintenance")
						return probe.Err()
					}
				}
				if cause == "shutdown" {
					cancel()
					return context.Canceled
				}
				return errors.New("leader session lost")
			}, func(ctx context.Context) {
				close(workStarted)
				<-ctx.Done() // represents a slow launch or verification command
				stopped.Add(1)
			}, maintenance(leaseStarted), maintenance(cancelStarted))
			if stopped.Load() != 3 {
				t.Fatalf("leader returned before its work stopped: %d", stopped.Load())
			}
		})
	}
}
