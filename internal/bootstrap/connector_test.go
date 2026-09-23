package bootstrap

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestConnectorRequiresCompleteCredentialDelivery(t *testing.T) {
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	expected := Runtime{Actor: Actor{Atespace: "space", Name: "worker", UID: "uid"},
		TemplateUID: "template", Image: "image", SandboxClass: "gvisor",
		BootstrapPublicKey: base64.StdEncoding.EncodeToString(signer.Public().(ed25519.PublicKey)),
		WorkerPod:          "pod", WorkerPodUID: "pod-uid", WorkerPool: "pool",
		SnapshotOnPause: "SNAPSHOT_CONTENT_SCOPE_DATA", SnapshotOnCommit: "SNAPSHOT_CONTENT_SCOPE_DATA",
		ResumeFromData: "RESUME_SOURCE_GOLDEN", SnapshotStorage: "store"}
	connector := Connector{Ledger: &Ledger{}, Client: http.DefaultClient, RouterURL: "https://router.example",
		Token: func(context.Context) (string, error) { return "token", nil }, Roots: x509.NewCertPool(), Signer: signer,
		Current:   func(context.Context) (Runtime, error) { return expected, nil },
		Authorize: func(context.Context, pgx.Tx, Runtime) error { return nil }}
	connector.GitSetup = func(context.Context, pgx.Tx, Runtime) (GitSetup, error) { return GitSetup{}, nil }
	if err := connector.Open(t.Context(), Scope{}, expected); !errors.Is(err, ErrDenied) {
		t.Fatalf("credential delivery without lease reservation accepted: %v", err)
	}
	connector.Reserve = func(context.Context, pgx.Tx, Redeemed) error { return nil }
	if err := connector.Open(t.Context(), Scope{}, expected); !errors.Is(err, ErrDenied) {
		t.Fatalf("credential delivery without lease acknowledgement accepted: %v", err)
	}
}

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

func TestRuntimeDataOnlySnapshots(t *testing.T) {
	valid := Runtime{SnapshotOnPause: "SNAPSHOT_CONTENT_SCOPE_DATA", SnapshotOnCommit: "SNAPSHOT_CONTENT_SCOPE_DATA",
		ResumeFromData: "RESUME_SOURCE_GOLDEN", SnapshotStorage: "gs://test/"}
	if !valid.DataOnlySnapshots() {
		t.Fatal("data-only snapshot policy rejected")
	}
	for name, change := range map[string]func(*Runtime){
		"pause memory":  func(r *Runtime) { r.SnapshotOnPause = "SNAPSHOT_CONTENT_SCOPE_ALL" },
		"commit memory": func(r *Runtime) { r.SnapshotOnCommit = "SNAPSHOT_CONTENT_SCOPE_ALL" },
		"resume memory": func(r *Runtime) { r.ResumeFromData = "RESUME_SOURCE_SNAPSHOT" },
		"unknown store": func(r *Runtime) { r.SnapshotStorage = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			change(&candidate)
			if candidate.DataOnlySnapshots() {
				t.Fatal("unsafe snapshot policy accepted")
			}
		})
	}
}

func TestGitCommitMustBeCanonicalHash(t *testing.T) {
	valid := "0123456789abcdef0123456789abcdef01234567"
	if !validGitCommit(valid) {
		t.Fatal("Git SHA-1 rejected")
	}
	for _, bad := range []string{"main", valid[:39], "g" + valid[1:], "A" + valid[1:]} {
		if validGitCommit(bad) {
			t.Fatalf("invalid Git commit accepted: %q", bad)
		}
	}
}
