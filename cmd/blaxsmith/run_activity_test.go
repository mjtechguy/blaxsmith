package main

import (
	"net/http/httptest"
	"testing"
)

func TestActivityStreamBoundsAndCursor(t *testing.T) {
	hub := newActivityHub(nil)
	leave := make([]func(), 0, activityStreamLimit)
	for range activityStreamLimit {
		stop, ok := hub.enter()
		if !ok {
			t.Fatal("stream slot refused before limit")
		}
		leave = append(leave, stop)
	}
	if _, ok := hub.enter(); ok {
		t.Fatal("stream limit exceeded")
	}
	_, unsubscribe := hub.subscribe("org:run")
	hub.wake("org:run")
	unsubscribe()
	leave[0]()
	if stop, ok := hub.enter(); !ok {
		t.Fatal("released slot unavailable")
	} else {
		stop()
	}
	for _, stop := range leave[1:] {
		stop()
	}
	for _, raw := range []string{"-1", "1x", "", "99999999999999999999"} {
		request := httptest.NewRequest("GET", "/?after="+raw, nil)
		if _, err := activityCursor(request); err == nil {
			t.Fatalf("invalid cursor %q accepted", raw)
		}
	}
	request := httptest.NewRequest("GET", "/?after=1", nil)
	request.Header.Set("Last-Event-ID", "2")
	if got, err := activityCursor(request); err != nil || got != 2 {
		t.Fatalf("reconnect cursor = %d, %v", got, err)
	}
}
