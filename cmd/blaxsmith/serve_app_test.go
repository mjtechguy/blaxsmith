package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/identity"
)

func TestServeAppConfig(t *testing.T) {
	t.Setenv("BLAXSMITH_DATABASE_URL", "postgres://user:password@db.example/blaxsmith?sslmode=disable")
	args := []string{"--listen", "127.0.0.1:8443", "--origin", "https://example.com", "--tls-cert-file", "cert", "--tls-key-file", "key", "--signer-file", "seed"}
	if _, err := parseAppConfig(nil); err == nil {
		t.Fatal("missing HTTPS configuration accepted")
	}
	for _, origin := range []string{"http://example.com", "https://example.com/path", "https://example.com?x=1"} {
		copyArgs := append([]string(nil), args...)
		copyArgs[3] = origin
		if _, err := parseAppConfig(copyArgs); err == nil {
			t.Fatalf("insecure origin accepted: %s", origin)
		}
	}
	if _, err := parseAppConfig(args); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		dsn, allowLocal string
		want            bool
	}{
		{"postgres://u:p@db.example/app?sslmode=disable", "false", false},
		{"postgres://u:p@db.example/app?sslmode=prefer", "false", false},
		{"postgres://u:p@db.example/app?sslmode=require", "false", false},
		{"postgres://u:p@db.example/app?sslmode=verify-full", "false", true},
		{"postgres://u:p@127.0.0.1/app?sslmode=disable", "false", false},
		{"postgres://u:p@127.0.0.1/app?sslmode=disable", "true", true},
		{"postgres://u:p@db.example/app?sslmode=disable", "true", false},
	} {
		config, err := pgxpool.ParseConfig(test.dsn)
		if err != nil {
			t.Fatal(err)
		}
		got := validateDatabaseTransport(config, test.allowLocal == "true") == nil
		if got != test.want {
			t.Errorf("database transport %q local=%s accepted=%v, want %v", test.dsn, test.allowLocal, got, test.want)
		}
	}
	certFile, keyFile, _ := writeAppTestCertificate(t, t.TempDir())
	if err := os.Chmod(keyFile, 0o644); err != nil {
		t.Fatal(err)
	}
	appArgs := []string{"--listen", "127.0.0.1:8443", "--origin", "https://example.com",
		"--tls-cert-file", certFile, "--tls-key-file", keyFile, "--signer-file", "seed"}
	if err := serveAppContext(context.Background(), appArgs); err == nil || !strings.Contains(err.Error(), "private key") {
		t.Fatalf("world-readable HTTPS private key accepted: %v", err)
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := serveAppContext(context.Background(), appArgs); err == nil || !strings.Contains(err.Error(), "public origin") {
		t.Fatalf("HTTPS certificate for wrong host accepted: %v", err)
	}
}

func TestServeAppHTTPSPostgres(t *testing.T) {
	dsn := os.Getenv("BLAXSMITH_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BLAXSMITH_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	schema := "blaxsmith_app_" + hex.EncodeToString(random[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanCtx, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	databaseURL, err := url.Parse(dsn)
	if err != nil || (databaseURL.Scheme != "postgres" && databaseURL.Scheme != "postgresql") {
		t.Fatal("BLAXSMITH_TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	query := databaseURL.Query()
	query.Set("search_path", schema)
	databaseURL.RawQuery = query.Encode()
	t.Setenv("BLAXSMITH_DATABASE_URL", databaseURL.String())
	pool, err := pgxpool.New(ctx, databaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	password := []byte("correct horse battery staple")
	owner, err := identity.BootstrapOwner(ctx, pool, "alice", "engineering", "Engineering", password)
	if err != nil {
		t.Fatal(err)
	}
	var oldHash [32]byte
	if _, err := rand.Read(oldHash[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_login_limits(scope,key_hash,window_start,attempts)
		VALUES ('source',$1,clock_timestamp()-interval '2 days',1)`, oldHash[:]); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	staticDir := filepath.Join(dir, "web")
	if err := os.MkdirAll(filepath.Join(staticDir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<title>Blaxsmith</title>"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "assets", "app.js"), []byte("export const ready = true;"), 0o600); err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, roots := writeAppTestCertificate(t, dir)
	signerFile := filepath.Join(dir, "signer.seed")
	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(signerFile, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	origin := "https://" + address
	appArgs := []string{"--listen", address, "--origin", origin,
		"--tls-cert-file", certFile, "--tls-key-file", keyFile, "--signer-file", signerFile,
		"--static-dir", staticDir,
		"--allow-insecure-local-database"}
	if _, err := pool.Exec(ctx, `INSERT INTO blaxsmith_schema_migrations(version,sha256) VALUES ('future','invalid')`); err != nil {
		t.Fatal(err)
	}
	if err := serveAppContext(ctx, appArgs); err == nil || !strings.Contains(err.Error(), "unknown to this binary") {
		t.Fatalf("unknown database migration did not block startup: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM blaxsmith_schema_migrations WHERE version='future'`); err != nil {
		t.Fatal(err)
	}
	appCtx, stop := context.WithCancel(ctx)
	defer stop()
	result := make(chan error, 1)
	go func() {
		result <- serveAppContext(appCtx, appArgs)
	}()
	client := &http.Client{Timeout: 3 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}}
	client.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		response, err := client.Get(origin + "/healthz")
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-result:
			t.Fatalf("app failed before readiness: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("app did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_login_limits WHERE key_hash=$1`, oldHash[:]).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("old login limit was not pruned: %d, %v", remaining, err)
	}
	tls12 := &http.Client{Timeout: 2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MaxVersion: tls.VersionTLS12}}}
	if response, err := tls12.Get(origin + "/healthz"); err == nil {
		response.Body.Close()
		t.Fatal("TLS 1.2 was accepted")
	}
	apiClient := apiv1connect.NewAuthServiceClient(client, origin+"/api")
	csrfRequest := connect.NewRequest(&api.GetCsrfRequest{})
	csrfRequest.Header().Set("Origin", origin)
	csrf, err := apiClient.GetCsrf(ctx, csrfRequest)
	if err != nil {
		t.Fatal(err)
	}
	login := connect.NewRequest(&api.LoginLocalRequest{OrganizationSlug: "engineering", Username: "alice", Password: string(password)})
	login.Header().Set("Origin", origin)
	login.Header().Set("X-Blaxsmith-CSRF", csrf.Msg.Token)
	login.Header().Set("X-Forwarded-For", "198.51.100.1")
	loggedIn, err := apiClient.LoginLocal(ctx, login)
	if err != nil || loggedIn.Msg.Session.PrincipalId != owner.PrincipalID {
		t.Fatalf("HTTPS login failed: %+v, %v", loggedIn, err)
	}
	realHash := sha256.Sum256([]byte("source\x00127.0.0.1"))
	spoofedHash := sha256.Sum256([]byte("source\x00198.51.100.1"))
	var real, spoofed int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_login_limits WHERE scope='source' AND key_hash=$1`, realHash[:]).Scan(&real); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_login_limits WHERE scope='source' AND key_hash=$1`, spoofedHash[:]).Scan(&spoofed); err != nil {
		t.Fatal(err)
	}
	if real != 1 || spoofed != 0 {
		t.Fatalf("forwarded IP affected login limits: real=%d spoofed=%d", real, spoofed)
	}
	for path, want := range map[string]int{"/": http.StatusOK, "/tools": http.StatusOK,
		"/assets/app.js": http.StatusOK, "/assets/missing.js": http.StatusNotFound,
		"/api/missing": http.StatusNotFound} {
		response, err := client.Get(origin + path)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != want {
			t.Fatalf("%s: HTTP %d, want %d", path, response.StatusCode, want)
		}
	}
	catalogResponse, err := client.Get(origin + "/api" + apiv1connect.CatalogServiceListToolsProcedure)
	if err != nil {
		t.Fatal(err)
	}
	catalogResponse.Body.Close()
	if catalogResponse.StatusCode == http.StatusNotFound {
		t.Fatal("catalog was not mounted on the application origin")
	}
	stop()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("graceful shutdown failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("app did not shut down")
	}
	manager, err := identity.NewSessionManager(pool, origin, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := newAppHandler(pool, manager, origin, staticDir)
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	unavailable := httptest.NewRecorder()
	handler.ServeHTTP(unavailable, httptest.NewRequest(http.MethodGet, origin+"/healthz", nil))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("health check ignored database outage: %d", unavailable.Code)
	}
	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, origin+"/livez", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("liveness incorrectly depends on database: %d", live.Code)
	}
}

func writeAppTestCertificate(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &private.PublicKey, private)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("failed to trust test certificate")
	}
	return certFile, keyFile, roots
}
