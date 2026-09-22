package bootstrap

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestConnectorDoesNotFollowRedirectWithToken(t *testing.T) {
	forwarded := false
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	router := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing connector token")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer router.Close()
	parsed, err := url.Parse(router.URL)
	if err != nil {
		t.Fatal(err)
	}
	connector := Connector{Client: router.Client(), Token: func(context.Context) (string, error) { return "test-token", nil }}
	_, _, _, err = connector.request(context.Background(), parsed, Offer{ActorAtespace: "a", ActorName: "b", ActorUID: "c"}, http.MethodGet, "/blaxsmith/bootstrap/challenge", nil)
	if err == nil || errors.Is(err, ErrDenied) || forwarded {
		t.Fatalf("redirect was followed or accepted: err=%v, forwarded=%t", err, forwarded)
	}
}
