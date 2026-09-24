package dispatch

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
)

type activatorActor struct{}

func (activatorActor) Current(context.Context, string, string) (bootstrap.Runtime, error) {
	return bootstrap.Runtime{}, nil
}
func (activatorActor) Gone(context.Context, string, string) (bool, error) { return false, nil }

func TestModelActivatorRequiresVerifiedRouterTLS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	u, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: u.Hostname()}}
	activator := &ModelActivator{DB: new(pgxpool.Pool), Secrets: new(access.SecretStore),
		ClusterID: "cluster-a", LeaseTTL: time.Minute, Actor: activatorActor{},
		Base: bootstrap.Connector{Client: &http.Client{Transport: transport, Timeout: 10 * time.Second},
			RouterURL: server.URL, Token: func(context.Context) (string, error) { return "token", nil },
			Roots: roots, Signer: signer}}
	if !activator.ready() {
		t.Fatal("verified private router transport was rejected")
	}
	transport.Proxy = http.ProxyFromEnvironment
	if activator.ready() {
		t.Fatal("environment proxy could redirect connector bootstrap traffic")
	}
}
