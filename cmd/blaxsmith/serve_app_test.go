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
	"net/netip"
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
	"github.com/mjtechguy/blaxsmith/internal/workflow"
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
	previousPublic, previousSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	previousFile := filepath.Join(dir, "previous.pub")
	if err := os.WriteFile(previousFile, previousPublic, 0o644); err != nil {
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
		"--previous-signer-public-file", previousFile,
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
	previousManager, err := identity.NewSessionManager(pool, origin, previousSigner)
	if err != nil {
		t.Fatal(err)
	}
	previousTokens, err := previousManager.LoginLocal(ctx, "engineering", "alice", password, netip.MustParseAddr("192.0.2.77"))
	if err != nil {
		t.Fatal(err)
	}
	previousClient := apiv1connect.NewAuthServiceClient(&http.Client{Timeout: 3 * time.Second, Transport: client.Transport}, origin+"/api")
	current := connect.NewRequest(&api.CurrentSessionRequest{})
	current.Header().Set("Origin", origin)
	current.Header().Set("Cookie", "__Host-blaxsmith_access="+previousTokens.Access)
	if response, err := previousClient.CurrentSession(ctx, current); err != nil || response.Msg.Session.PrincipalId != owner.PrincipalID {
		t.Fatalf("previous signing key was not accepted during overlap: %+v, %v", response, err)
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
	testWorkflowBrowserAPI(t, ctx, pool, client, origin, csrf.Msg.Token, owner, password, seed)
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
	case <-time.After(15 * time.Second):
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

func testWorkflowBrowserAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *http.Client,
	origin, csrf string, owner identity.FirstOwner, password, seed []byte) {
	t.Helper()
	w := apiv1connect.NewWorkflowServiceClient(client, origin+"/api")
	create := func(slug string) *api.Project {
		t.Helper()
		req := connect.NewRequest(&api.CreateProjectRequest{Slug: slug, Name: slug})
		req.Header().Set("Origin", origin)
		req.Header().Set("X-Blaxsmith-CSRF", csrf)
		got, err := w.CreateProject(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		return got.Msg.Project
	}
	denied := connect.NewRequest(&api.CreateProjectRequest{Slug: "denied", Name: "Denied"})
	denied.Header().Set("Origin", origin)
	if _, err := w.CreateProject(ctx, denied); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("mutation without CSRF allowed: %v", err)
	}
	denied.Header().Set("Origin", "https://outside.example")
	denied.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.CreateProject(ctx, denied); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("mutation from another origin allowed: %v", err)
	}
	first, second := create("first-project"), create("second-project")
	var createdAudit int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity_audit_events
		WHERE organization_id=$1 AND actor_id=$2 AND action='workflow.project.created'
		AND subject_id IN ($3,$4)`, owner.OrganizationID, owner.PrincipalID, first.Id, second.Id).Scan(&createdAudit); err != nil || createdAudit != 2 {
		t.Fatalf("project creation audit: %d, %v", createdAudit, err)
	}
	getProject := connect.NewRequest(&api.GetProjectRequest{ProjectId: first.Id})
	getProject.Header().Set("Origin", origin)
	if got, err := w.GetProject(ctx, getProject); err != nil || got.Msg.Project.Id != first.Id {
		t.Fatalf("own project lookup: %+v, %v", got, err)
	}
	page := connect.NewRequest(&api.ListProjectsRequest{PageSize: 1})
	page.Header().Set("Origin", origin)
	pageOne, err := w.ListProjects(ctx, page)
	if err != nil || len(pageOne.Msg.Projects) != 1 || pageOne.Msg.Projects[0].Id != second.Id || pageOne.Msg.NextPageToken == "" {
		t.Fatalf("first project page: %+v, %v", pageOne, err)
	}
	page.Msg.PageToken = pageOne.Msg.NextPageToken
	pageTwo, err := w.ListProjects(ctx, page)
	if err != nil || len(pageTwo.Msg.Projects) != 1 || pageTwo.Msg.Projects[0].Id != first.Id {
		t.Fatalf("second project page: %+v, %v", pageTwo, err)
	}
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: owner.OrganizationID, ProjectID: first.Id,
		LaunchKey: "api-read", SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	getRun := connect.NewRequest(&api.GetRunRequest{RunId: run.ID})
	getRun.Header().Set("Origin", origin)
	if got, err := w.GetRun(ctx, getRun); err != nil || got.Msg.Run.Id != run.ID {
		t.Fatalf("own run lookup: %+v, %v", got, err)
	}
	listRuns := connect.NewRequest(&api.ListRunsRequest{ProjectId: first.Id})
	listRuns.Header().Set("Origin", origin)
	if got, err := w.ListRuns(ctx, listRuns); err != nil || len(got.Msg.Runs) != 1 || got.Msg.Runs[0].Id != run.ID {
		t.Fatalf("own run list: %+v, %v", got, err)
	}
	events := connect.NewRequest(&api.EventsAfterRequest{RunId: run.ID})
	events.Header().Set("Origin", origin)
	if got, err := w.EventsAfter(ctx, events); err != nil || len(got.Msg.Events) != 1 || got.Msg.Events[0].Kind != "run.created" {
		t.Fatalf("own events: %+v, %v", got, err)
	}
	var otherOrg string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'another-org','Another') RETURNING id`).Scan(&otherOrg); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity_memberships (organization_id,principal_id,role)
		VALUES ($1,$2,'owner')`, otherOrg, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	manager, err := identity.NewSessionManager(pool, origin, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	otherTokens, err := manager.LoginLocal(ctx, "another-org", "alice", password, netip.MustParseAddr("192.0.2.55"))
	if err != nil {
		t.Fatal(err)
	}
	other := apiv1connect.NewWorkflowServiceClient(&http.Client{Timeout: 3 * time.Second, Transport: client.Transport}, origin+"/api")
	cookie := "__Host-blaxsmith_access=" + otherTokens.Access
	otherProjects := connect.NewRequest(&api.ListProjectsRequest{})
	otherProjects.Header().Set("Origin", origin)
	otherProjects.Header().Set("Cookie", cookie)
	if got, err := other.ListProjects(ctx, otherProjects); err != nil || len(got.Msg.Projects) != 0 {
		t.Fatalf("cross-tenant project list: %+v, %v", got, err)
	}
	getProject.Header().Set("Cookie", cookie)
	if _, err := other.GetProject(ctx, getProject); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant project lookup: %v", err)
	}
	getRun.Header().Set("Cookie", cookie)
	if _, err := other.GetRun(ctx, getRun); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant run lookup: %v", err)
	}
	listRuns.Header().Set("Cookie", cookie)
	if _, err := other.ListRuns(ctx, listRuns); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant run list: %v", err)
	}
	events.Header().Set("Cookie", cookie)
	if _, err := other.EventsAfter(ctx, events); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant events: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_memberships SET role='viewer'
		WHERE organization_id=$1 AND principal_id=$2`, owner.OrganizationID, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	refresh := connect.NewRequest(&api.RefreshSessionRequest{})
	refresh.Header().Set("Origin", origin)
	refresh.Header().Set("X-Blaxsmith-CSRF", csrf)
	auth := apiv1connect.NewAuthServiceClient(client, origin+"/api")
	if got, err := auth.RefreshSession(ctx, refresh); err != nil || got.Msg.Session.Role != "viewer" {
		t.Fatalf("viewer refresh: %+v, %v", got, err)
	}
	viewerCreate := connect.NewRequest(&api.CreateProjectRequest{Slug: "viewer-denied", Name: "Denied"})
	viewerCreate.Header().Set("Origin", origin)
	viewerCreate.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.CreateProject(ctx, viewerCreate); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer created project: %v", err)
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
