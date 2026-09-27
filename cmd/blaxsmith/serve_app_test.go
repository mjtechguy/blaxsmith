package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/runnerexit"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func TestServeAppConfig(t *testing.T) {
	t.Setenv("BLAXSMITH_DATABASE_URL", "postgres://user:password@db.example/blaxsmith?sslmode=disable")
	if err := migrateDatabase([]string{"--require-verified-database"}); err == nil || !strings.Contains(err.Error(), "verified TLS") {
		t.Fatalf("migration accepted unverified database transport: %v", err)
	}
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
	if config, err := parseAppConfig(append(args, "--migrations", "verify")); err != nil || config.migrations != "verify" {
		t.Fatalf("verify-only configuration rejected: %+v, %v", config, err)
	}
	if _, err := parseAppConfig(append(args, "--migrations", "ignore")); err == nil {
		t.Fatal("invalid migration mode accepted")
	}
	if config, err := parseAppConfig(args); err != nil || config.sessionPolicy != identity.DefaultSessionPolicy || config.shutdownTimeout != 20*time.Second {
		t.Fatalf("session and shutdown defaults: %+v, %v", config, err)
	}
	if config, err := parseAppConfig(append(args, "--session-idle-timeout", "8h", "--session-absolute-lifetime", "72h",
		"--shutdown-timeout", "45s")); err != nil || config.sessionPolicy.Idle != 8*time.Hour ||
		config.sessionPolicy.Absolute != 72*time.Hour || config.shutdownTimeout != 45*time.Second {
		t.Fatalf("session policy flags: %+v, %v", config, err)
	}
	for _, bad := range [][]string{{"--session-idle-timeout", "1m"}, {"--session-idle-timeout", "48h", "--session-absolute-lifetime", "24h"},
		{"--session-absolute-lifetime", "9000h"}, {"--shutdown-timeout", "0s"}} {
		if _, err := parseAppConfig(append(append([]string(nil), args...), bad...)); err == nil {
			t.Fatalf("invalid lifetime flags accepted: %v", bad)
		}
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
	ctx := tenant.System(context.Background()) // fixtures and assertions read every organization
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
	pool, err := tenant.NewPool(ctx, databaseURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	password := []byte("correct horse battery staple")
	owner, err := identity.BootstrapOwner(ctx, pool, "alice@example.com", "engineering", "Engineering", password)
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
	oauthClients := filepath.Join(dir, "oauth-clients.json")
	if err := os.WriteFile(oauthClients, []byte(`[{"client_id":"test-mcp","client_name":"E2E MCP","redirect_uris":["https://client.example.test/callback","http://127.0.0.1:8123/callback"]},{"client_id":"other-mcp","client_name":"Other MCP","redirect_uris":["https://other.example.test/callback"]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLAXSMITH_MCP_OAUTH_CLIENTS_FILE", oauthClients)
	staticDir := filepath.Join(dir, "web")
	if err := os.MkdirAll(filepath.Join(staticDir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("BLAXSMITH_E2E_BROWSER") == "1" {
		if err := os.CopyFS(staticDir, os.DirFS("../../frontend/dist")); err != nil {
			t.Fatalf("build frontend before browser E2E: %v", err)
		}
	} else if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<title>Blaxsmith</title>"), 0o600); err != nil {
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
	var missingVersion, missingDigest string
	if err := pool.QueryRow(ctx, `SELECT version,sha256 FROM blaxsmith_schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&missingVersion, &missingDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM blaxsmith_schema_migrations WHERE version=$1`, missingVersion); err != nil {
		t.Fatal(err)
	}
	if err := serveAppContext(ctx, append(appArgs, "--migrations", "verify")); err == nil || !strings.Contains(err.Error(), "is not applied") {
		t.Fatalf("verify-only app accepted missing migration: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO blaxsmith_schema_migrations(version,sha256) VALUES ($1,$2)`, missingVersion, missingDigest); err != nil {
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
	// A second process shares the public origin and database, as it would
	// behind an ingress. The request target differs, but the Host stays public.
	secondListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	secondAddress := secondListener.Addr().String()
	secondListener.Close()
	secondArgs := append([]string(nil), appArgs...)
	secondArgs[1] = secondAddress
	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	secondResult := make(chan error, 1)
	go func() { secondResult <- serveAppContext(secondCtx, secondArgs) }()
	secondOrigin := "https://" + secondAddress
	for {
		request, err := http.NewRequest(http.MethodGet, secondOrigin+"/healthz", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Host = address
		response, err := client.Do(request)
		if err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				break
			}
		}
		select {
		case err := <-secondResult:
			t.Fatalf("second app failed before readiness: %v", err)
		default:
		}
		if time.Now().After(deadline.Add(8 * time.Second)) {
			t.Fatal("second app did not become ready")
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
	previousTokens, err := previousManager.LoginLocal(ctx, "engineering", "alice@example.com", password, netip.MustParseAddr("192.0.2.77"), "")
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
	login := connect.NewRequest(&api.LoginLocalRequest{OrganizationSlug: "engineering", Email: "alice@example.com", Password: string(password)})
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
	testWorkflowBrowserAPI(t, ctx, pool, client, origin, secondOrigin, csrf.Msg.Token, owner, password, seed)
	testAccountBrowserAPI(t, ctx, pool, client.Transport, origin, password)
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
	stopSecond()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("graceful shutdown failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("app did not shut down")
	}
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second app shutdown failed: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("second app did not shut down")
	}
	manager, err := identity.NewSessionManager(pool, origin, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	drain := newDrainer()
	handler, product, err := newAppHandler(ctx, pool, manager, origin, staticDir, nil, dispatchConfig{}, nil, nil, drain)
	if err != nil {
		t.Fatal(err)
	}
	if product != nil {
		product.Close()
	}
	ready := httptest.NewRecorder()
	handler.ServeHTTP(ready, httptest.NewRequest(http.MethodGet, origin+"/healthz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("readiness before drain: %d", ready.Code)
	}
	pool.Close()
	unavailable := httptest.NewRecorder()
	handler.ServeHTTP(unavailable, httptest.NewRequest(http.MethodGet, origin+"/healthz", nil))
	if unavailable.Code != http.StatusServiceUnavailable || !strings.Contains(unavailable.Body.String(), "database") {
		t.Fatalf("health check ignored database outage: %d", unavailable.Code)
	}
	live := httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, origin+"/livez", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("liveness incorrectly depends on database: %d", live.Code)
	}
	// SIGTERM: readiness fails at once so traffic moves away; liveness holds
	// so the kubelet does not kill the pod mid-drain.
	drain.begin()
	draining := httptest.NewRecorder()
	handler.ServeHTTP(draining, httptest.NewRequest(http.MethodGet, origin+"/healthz", nil))
	if draining.Code != http.StatusServiceUnavailable || !strings.Contains(draining.Body.String(), "shutting down") {
		t.Fatalf("readiness passed while draining: %d", draining.Code)
	}
	live = httptest.NewRecorder()
	handler.ServeHTTP(live, httptest.NewRequest(http.MethodGet, origin+"/livez", nil))
	if live.Code != http.StatusOK {
		t.Fatalf("liveness failed while draining: %d", live.Code)
	}
}

func testWorkflowBrowserAPI(t *testing.T, ctx context.Context, pool *pgxpool.Pool, client *http.Client,
	origin, secondOrigin, csrf string, owner identity.FirstOwner, password, seed []byte) {
	t.Helper()
	w := apiv1connect.NewWorkflowServiceClient(client, origin+"/api")
	capsReq := connect.NewRequest(&api.GetPlatformCapabilitiesRequest{})
	capsReq.Header().Set("Origin", origin)
	caps, err := w.GetPlatformCapabilities(ctx, capsReq)
	if err != nil || !caps.Msg.LaunchPreview || caps.Msg.MaxImplementStages != 1 {
		t.Fatalf("capabilities: %v %v", caps, err)
	}
	previewReq := connect.NewRequest(&api.PreviewRunRequest{RecipePath: "recipe.json", RecipeVersionId: "also-selected", Scope: "."})
	previewReq.Header().Set("Origin", origin)
	if _, err := w.PreviewRun(ctx, previewReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("preview without CSRF: %v", err)
	}
	previewReq.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.PreviewRun(ctx, previewReq); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("ambiguous preview selection: %v", err)
	}
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
	goalReq := connect.NewRequest(&api.CreateGoalRequest{ProjectId: first.Id, RequestKey: "browser-goal", Title: "A native goal", Brief: "Preserve existing behavior."})
	goalReq.Header().Set("Origin", origin)
	if _, err := w.CreateGoal(ctx, goalReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("goal without CSRF: %v", err)
	}
	goalReq.Header().Set("X-Blaxsmith-CSRF", csrf)
	createdGoal, err := w.CreateGoal(ctx, goalReq)
	if err != nil || createdGoal.Msg.Goal.FactoryId != "anvil" || len(createdGoal.Msg.Goal.Questions) != 4 {
		t.Fatalf("create goal: %v %v", createdGoal, err)
	}
	goalRead := connect.NewRequest(&api.GetGoalRequest{GoalId: createdGoal.Msg.Goal.Id})
	goalRead.Header().Set("Origin", origin)
	loadedGoal, err := w.GetGoal(ctx, goalRead)
	if err != nil || loadedGoal.Msg.Goal.Revision != 1 {
		t.Fatalf("read goal: %v %v", loadedGoal, err)
	}
	goalReply := connect.NewRequest(&api.ReplyGoalRequest{GoalId: createdGoal.Msg.Goal.Id, RequestKey: "browser-reply", ExpectedRevision: 1, Kind: "message", Text: "Use existing conventions."})
	goalReply.Header().Set("Origin", origin)
	if _, err := w.ReplyGoal(ctx, goalReply); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("goal reply without CSRF: %v", err)
	}
	goalReply.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.ReplyGoal(ctx, goalReply); err != nil {
		t.Fatal(err)
	}
	goalReply.Msg.RequestKey = "late-reply"
	if _, err := w.ReplyGoal(ctx, goalReply); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale goal reply: %v", err)
	}
	goalList := connect.NewRequest(&api.ListGoalsRequest{ProjectId: second.Id})
	goalList.Header().Set("Origin", origin)
	listedGoals, err := w.ListGoals(ctx, goalList)
	if err != nil || len(listedGoals.Msg.Goals) != 0 {
		t.Fatalf("goal list project scope: %v %v", listedGoals, err)
	}
	planJSON := `{"schema_version":"anvil.plan/v1alpha1","title":"Preserve behavior","summary":"Follow existing conventions","requirements":[{"id":"R1","description":"Preserve behavior","sources":["brief","message:2"],"examples":["Existing requests still succeed"]}],"phases":[{"id":"P1","title":"Deliver","outcome":"Behavior preserved"}],"tasks":[{"id":"T1","title":"Implement","phase":"P1","reason":"Meet the goal","requirement_ids":["R1"],"instructions":"Use the existing service","acceptance":["Existing behavior works"]}]}`
	savePlan := connect.NewRequest(&api.SaveGoalPlanRequest{GoalId: createdGoal.Msg.Goal.Id, RequestKey: "save-plan", ExpectedGoalRevision: 2, ContentJson: planJSON})
	savePlan.Header().Set("Origin", origin)
	if _, err := w.SaveGoalPlan(ctx, savePlan); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("plan save without CSRF: %v", err)
	}
	savePlan.Header().Set("X-Blaxsmith-CSRF", csrf)
	for range 2 {
		saved, err := w.SaveGoalPlan(ctx, savePlan)
		if err != nil || saved.Msg.Version != 1 {
			t.Fatalf("plan save/retry: %v %v", saved, err)
		}
	}
	savePlan.Msg.RequestKey = "stale-plan"
	if _, err := w.SaveGoalPlan(ctx, savePlan); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale plan overwritten: %v", err)
	}
	savePlan.Msg.ExpectedPlanVersion = 1
	savePlan.Msg.ContentJson = strings.Replace(planJSON, "message:2", "message:999", 1)
	if _, err := w.SaveGoalPlan(ctx, savePlan); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("fabricated citation accepted: %v", err)
	}
	readPlans := connect.NewRequest(&api.GetGoalPlansRequest{GoalId: createdGoal.Msg.Goal.Id})
	readPlans.Header().Set("Origin", origin)
	plans, err := w.GetGoalPlans(ctx, readPlans)
	if err != nil || len(plans.Msg.Plans) != 1 || plans.Msg.Plans[0].GoalRevision != 2 || len(plans.Msg.Runs) != 0 {
		t.Fatalf("plan readback: %v %v", plans, err)
	}
	startPlan := connect.NewRequest(&api.StartGoalPlanningRequest{GoalId: createdGoal.Msg.Goal.Id, ExpectedRevision: 2, RequestKey: "start-plan", Harness: "codex", Model: "test-model", Effort: "medium", Scope: ".", RuntimeSeconds: 900})
	startPlan.Header().Set("Origin", origin)
	if _, err := w.StartGoalPlanning(ctx, startPlan); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("planner without CSRF: %v", err)
	}
	startPlan.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.StartGoalPlanning(ctx, startPlan); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("planner bypassed offline dispatcher: %v", err)
	}

	for _, skill := range []bool{false, true} {
		if skill {
			startPlan.Msg.SkillFiles = []string{"../outside/SKILL.md"}
		} else {
			startPlan.Msg.InstructionFiles = []string{"../outside.md"}
		}
		if _, err := w.StartGoalPlanning(ctx, startPlan); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("planner ignored invalid rule/skill selection: %v", err)
		}
		startPlan.Msg.InstructionFiles, startPlan.Msg.SkillFiles = nil, nil
	}

	execution := &api.GoalExecutionOptions{GoalId: createdGoal.Msg.Goal.Id, ExpectedGoalRevision: 2, PlanVersion: 1, Harness: "codex", Model: "test-model", Effort: "medium", RuntimeSeconds: 900, Acceptance: "manual"}
	previewExecution := connect.NewRequest(&api.PreviewRunRequest{ProjectId: first.Id, Scope: ".", GoalExecution: execution})
	previewExecution.Header().Set("Origin", origin)
	if _, err := w.PreviewRun(ctx, previewExecution); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("execution preview without CSRF: %v", err)
	}
	previewExecution.Header().Set("X-Blaxsmith-CSRF", csrf)
	previewExecution.Msg.RecipePath = "recipe.json"
	if _, err := w.PreviewRun(ctx, previewExecution); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("mixed execution sources: %v", err)
	}
	previewExecution.Msg.RecipePath = ""
	previewExecution.Msg.ProjectId = second.Id
	if _, err := w.PreviewRun(ctx, previewExecution); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-project execution: %v", err)
	}
	previewExecution.Msg.ProjectId = first.Id
	execution.ExpectedGoalRevision = 1
	if _, err := w.PreviewRun(ctx, previewExecution); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale execution context: %v", err)
	}
	execution.ExpectedGoalRevision = 2
	for _, skill := range []bool{false, true} {
		if skill {
			execution.SkillFiles = []string{"../outside/SKILL.md"}
		} else {
			execution.InstructionFiles = []string{"../outside.md"}
		}
		if _, err := w.PreviewRun(ctx, previewExecution); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("execution ignored invalid rule/skill selection: %v", err)
		}
		execution.InstructionFiles, execution.SkillFiles = nil, nil
	}
	launchExecution := connect.NewRequest(&api.LaunchRunRequest{ProjectId: first.Id, LaunchKey: "execution", Scope: ".", GoalExecution: execution})
	launchExecution.Header().Set("Origin", origin)
	launchExecution.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.LaunchRun(ctx, launchExecution); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("execution without reviewed hashes: %v", err)
	}

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
	page.Msg = &api.ListProjectsRequest{PageSize: 1, Search: "PROJECT", SortBy: "name", SortDirection: "asc"}
	byName, err := w.ListProjects(ctx, page)
	if err != nil || len(byName.Msg.Projects) != 1 || byName.Msg.Projects[0].Id != first.Id || byName.Msg.NextPageToken == "" {
		t.Fatalf("server project search/sort first page: %+v, %v", byName, err)
	}
	page.Msg.PageToken = byName.Msg.NextPageToken
	byName, err = w.ListProjects(ctx, page)
	if err != nil || len(byName.Msg.Projects) != 1 || byName.Msg.Projects[0].Id != second.Id {
		t.Fatalf("server project search/sort second page: %+v, %v", byName, err)
	}
	page.Msg.Search = "first"
	if _, err := w.ListProjects(ctx, page); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("changed project search accepted old cursor: %v", err)
	}
	store, err := workflow.New(pool)
	if err != nil {
		t.Fatal(err)
	}
	bundleJSON, bundleSHA := apiReviewBundle()
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: owner.OrganizationID, ProjectID: first.Id,
		LaunchKey: "api-read", SourceCommit: strings.Repeat("a", 40), BundleSHA256: bundleSHA, VerificationSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(apiReviewPolicy)))})
	if err != nil {
		t.Fatal(err)
	}
	streamURL := origin + "/api/runs/" + run.ID + "/events?after=0"
	streamRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	streamRequest.Header.Set("Origin", origin)
	stream, err := client.Do(streamRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("run activity stream: HTTP %d %s", stream.StatusCode, stream.Header.Get("Content-Type"))
	}
	if stream.Header.Get("Cache-Control") != "no-store" || stream.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("stream buffering/cache headers: %v", stream.Header)
	}
	reader := bufio.NewReader(stream.Body)
	if got := readRunActivityEvent(t, reader); got.ID != "1" || got.Kind != "run.created" || got.RunID != run.ID {
		t.Fatalf("initial durable stream event: %+v", got)
	}
	secondRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, secondOrigin+"/api/runs/"+run.ID+"/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondRequest.Host = strings.TrimPrefix(origin, "https://")
	secondRequest.Header.Set("Origin", origin)
	secondStream, err := client.Do(secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer secondStream.Body.Close()
	if secondStream.StatusCode != http.StatusOK {
		t.Fatalf("second replica activity stream: HTTP %d", secondStream.StatusCode)
	}
	secondReader := bufio.NewReader(secondStream.Body)
	if got := readRunActivityEvent(t, secondReader); got.ID != "1" || got.Kind != "run.created" {
		t.Fatalf("second replica durable replay: %+v", got)
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
	otherRun, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: owner.OrganizationID, ProjectID: first.Id,
		LaunchKey: "zeta-read", SourceCommit: strings.Repeat("d", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		t.Fatal(err)
	}
	listRuns.Msg = &api.ListRunsRequest{ProjectId: first.Id, PageSize: 1, Search: "READ", SortBy: "launch_key", SortDirection: "asc"}
	byKey, err := w.ListRuns(ctx, listRuns)
	if err != nil || len(byKey.Msg.Runs) != 1 || byKey.Msg.Runs[0].Id != run.ID || byKey.Msg.NextPageToken == "" {
		t.Fatalf("server run search/sort first page: %+v, %v", byKey, err)
	}
	listRuns.Msg.PageToken = byKey.Msg.NextPageToken
	byKey, err = w.ListRuns(ctx, listRuns)
	if err != nil || len(byKey.Msg.Runs) != 1 || byKey.Msg.Runs[0].Id != otherRun.ID {
		t.Fatalf("server run search/sort second page: %+v, %v", byKey, err)
	}
	listRuns.Msg.SortBy = "source_commit"
	if _, err := w.ListRuns(ctx, listRuns); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unsupported run sort accepted: %v", err)
	}
	listRuns.Msg = &api.ListRunsRequest{ProjectId: first.Id}
	events := connect.NewRequest(&api.EventsAfterRequest{RunId: run.ID})
	events.Header().Set("Origin", origin)
	if got, err := w.EventsAfter(ctx, events); err != nil || len(got.Msg.Events) != 1 || got.Msg.Events[0].Kind != "run.created" {
		t.Fatalf("own events: %+v, %v", got, err)
	}
	exits := connect.NewRequest(&api.ListCommandExitsRequest{RunId: run.ID})
	exits.Header().Set("Origin", origin)
	if got, err := w.ListCommandExits(ctx, exits); err != nil || len(got.Msg.Observations) != 0 || got.Msg.NextAfterEventId != 0 {
		t.Fatalf("empty command exit feed: %+v, %v", got, err)
	}
	exits.Msg.AfterEventId = -1
	if _, err := w.ListCommandExits(ctx, exits); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid command exit cursor: %v", err)
	}
	exits.Msg.AfterEventId = 0
	exits.Header().Set("Origin", "https://outside.example")
	if _, err := w.ListCommandExits(ctx, exits); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-origin command exit feed: %v", err)
	}
	exits.Header().Set("Origin", origin)
	getReview := connect.NewRequest(&api.GetCurrentReviewRequest{RunId: run.ID})
	getReview.Header().Set("Origin", origin)
	if _, err := w.GetCurrentReview(ctx, getReview); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("empty review should not be presented: %v", err)
	}
	task, err := store.AddTask(ctx, owner.OrganizationID, run.ID, "implement", run.BundleSHA256, 1)
	if err != nil {
		t.Fatal(err)
	}
	runTasks := connect.NewRequest(&api.ListRunTasksRequest{RunId: run.ID})
	runTasks.Header().Set("Origin", origin)
	if got, err := w.ListRunTasks(ctx, runTasks); err != nil || len(got.Msg.Tasks) != 1 ||
		got.Msg.Tasks[0].Id != task || got.Msg.Tasks[0].Key != "implement" || got.Msg.Tasks[0].State != "pending" {
		t.Fatalf("own run graph: %+v, %v", got, err)
	}
	if got := readRunActivityEvent(t, reader); got.ID != "2" || got.Kind != "task.created" || got.TaskID != task {
		t.Fatalf("live committed stream event: %+v", got)
	}
	if got := readRunActivityEvent(t, secondReader); got.ID != "2" || got.Kind != "task.created" || got.TaskID != task {
		t.Fatalf("cross-replica notification did not wake stream: %+v", got)
	}
	secondReconnect, err := http.NewRequestWithContext(ctx, http.MethodGet, secondOrigin+"/api/runs/"+run.ID+"/events?after=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	secondReconnect.Host = strings.TrimPrefix(origin, "https://")
	secondReconnect.Header.Set("Origin", origin)
	secondReconnect.Header.Set("Last-Event-ID", "1")
	secondReplay, err := client.Do(secondReconnect)
	if err != nil {
		t.Fatal(err)
	}
	if got := readRunActivityEvent(t, bufio.NewReader(secondReplay.Body)); got.ID != "2" || got.Kind != "task.created" {
		t.Fatalf("second replica reconnect cursor replay: %+v", got)
	}
	secondReplay.Body.Close()
	reconnect, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	reconnect.Header.Set("Origin", origin)
	reconnect.Header.Set("Last-Event-ID", "1")
	replayed, err := client.Do(reconnect)
	if err != nil {
		t.Fatal(err)
	}
	if got := readRunActivityEvent(t, bufio.NewReader(replayed.Body)); got.ID != "2" || got.Kind != "task.created" {
		t.Fatalf("Last-Event-ID replay: %+v", got)
	}
	replayed.Body.Close()
	for _, test := range []struct {
		origin, cursor string
		want           int
	}{
		{"https://outside.example", "0", http.StatusUnauthorized},
		{"", "0", http.StatusUnauthorized},
		{origin, "999999", http.StatusBadRequest},
		{origin, "-1", http.StatusBadRequest},
	} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/runs/"+run.ID+"/events?after="+test.cursor, nil)
		if err != nil {
			t.Fatal(err)
		}
		if test.origin != "" {
			req.Header.Set("Origin", test.origin)
		}
		got, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		got.Body.Close()
		if got.StatusCode != test.want {
			t.Fatalf("stream origin/cursor check: HTTP %d, want %d", got.StatusCode, test.want)
		}
	}
	fetchMetadata, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	fetchMetadata.Header.Set("Sec-Fetch-Site", "same-origin")
	fetchResponse, err := client.Do(fetchMetadata)
	if err != nil {
		t.Fatal(err)
	}
	fetchResponse.Body.Close()
	if fetchResponse.StatusCode != http.StatusOK {
		t.Fatalf("native EventSource metadata denied: HTTP %d", fetchResponse.StatusCode)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_run_bundles(organization_id,run_id,bundle_json,verification_json) VALUES($1,$2,$3::jsonb,$4::jsonb)`, owner.OrganizationID, run.ID, bundleJSON, apiReviewPolicy); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true WHERE organization_id=$1 AND id=$2`, owner.OrganizationID, run.ID); err != nil {
		t.Fatal(err)
	}
	attempt, err := store.ReserveAttempt(ctx, owner.OrganizationID, run.ID, task)
	if err != nil {
		t.Fatal(err)
	}
	binding := workflow.RuntimeBinding{AXAtespace: "api-test", AXTask: "task-one", ActorUID: "actor-one",
		TemplateUID: "template-one", Image: "runner@sha256:" + strings.Repeat("a", 64),
		WorkerPool: "pool-one", CommandSHA256: strings.Repeat("b", 64)}
	if err := store.BindRuntime(ctx, attempt, binding); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarting(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmStarted(ctx, attempt); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	empty := sha256.Sum256(nil)
	report := runnerexit.ExitReport{Schema: runnerexit.Schema, OrganizationID: owner.OrganizationID,
		RunID: run.ID, TaskID: task, AttemptID: attempt.ID, OwnerGeneration: attempt.OwnerGeneration,
		AXAtespace: binding.AXAtespace, AXTask: binding.AXTask, ActorUID: binding.ActorUID,
		TemplateUID: binding.TemplateUID, ActivationNonce: base64.RawURLEncoding.EncodeToString(nonce[:]),
		CommandSHA256: binding.CommandSHA256, ExitCode: 0, Sequence: 1,
		EvidenceSHA256: hex.EncodeToString(empty[:]), ObservedAt: time.Now().UnixNano()}
	signed, err := runnerexit.Sign(report, private)
	if err != nil {
		t.Fatal(err)
	}
	collector := workflow.CommandExitCollector{Store: store, SignerID: "pool-one/connector", PublicKey: public, WorkerPool: "pool-one"}
	if err := collector.Record(ctx, signed); err != nil {
		t.Fatal(err)
	}
	observed, err := w.ListCommandExits(ctx, exits)
	if err != nil || len(observed.Msg.Observations) != 1 || observed.Msg.Observations[0].ExitCode != 0 ||
		observed.Msg.Observations[0].ActorUid != binding.ActorUID || observed.Msg.Observations[0].AttemptId != attempt.ID ||
		observed.Msg.Observations[0].SignerId != "pool-one/connector" ||
		observed.Msg.Observations[0].ReceiptSha256 == "" || observed.Msg.NextAfterEventId < 1 {
		t.Fatalf("signed command exit feed: %+v, %v", observed, err)
	}
	exits.Msg.AfterEventId = observed.Msg.NextAfterEventId
	if replay, err := w.ListCommandExits(ctx, exits); err != nil || len(replay.Msg.Observations) != 0 {
		t.Fatalf("command exit page cursor: %+v, %v", replay, err)
	}
	exits.Msg.AfterEventId = 0
	if err := store.FinishAttempt(ctx, attempt, true, run.BundleSHA256); err != nil {
		t.Fatal(err)
	}
	if state, err := store.FinalizeRun(ctx, owner.OrganizationID, run.ID); err != nil || state != "succeeded" {
		t.Fatalf("finish review fixture: %s, %v", state, err)
	}
	seedAPIReviewProof(t, pool, owner.OrganizationID, run.ID, strings.Repeat("d", 40))
	currentReview, err := store.PresentForReview(ctx, owner.OrganizationID, run.ID, strings.Repeat("d", 40), strings.Repeat("e", 64), run.VerificationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.GetCurrentReview(ctx, getReview); err != nil || got.Msg.Package.Id != currentReview.ID ||
		got.Msg.Package.EvidenceSha256 != currentReview.EvidenceSHA256 || got.Msg.Package.Decision != nil {
		t.Fatalf("current review package: %+v, %v", got, err)
	}
	// Associate the completed fixture with its saved plan, as frozen admission does.
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_goal_runs(organization_id,goal_id,run_id,goal_revision,plan_version) VALUES($1,$2,$3,2,1)`, owner.OrganizationID, createdGoal.Msg.Goal.Id, run.ID); err != nil {
		t.Fatal(err)
	}
	checkpoint := connect.NewRequest(&api.RecordGoalCheckpointRequest{GoalId: createdGoal.Msg.Goal.Id, RunId: run.ID, PackageId: currentReview.ID, Title: "Behavior preserved", RequestKey: "first-checkpoint", ExpectedGoalRevision: 2})
	checkpoint.Header().Set("Origin", origin)
	if _, err := w.RecordGoalCheckpoint(ctx, checkpoint); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("checkpoint without CSRF: %v", err)
	}
	checkpoint.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.RecordGoalCheckpoint(ctx, checkpoint); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("checkpoint invented acceptance: %v", err)
	}
	decision := connect.NewRequest(&api.DecideReviewRequest{RunId: run.ID, PackageId: currentReview.ID,
		IdempotencyKey: "browser-approve", Action: "approve"})
	decision.Header().Set("Origin", origin)
	if _, err := w.DecideReview(ctx, decision); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("review decision without CSRF accepted: %v", err)
	}
	decision.Header().Set("X-Blaxsmith-CSRF", csrf)
	decision.Header().Set("Origin", "https://outside.example")
	if _, err := w.DecideReview(ctx, decision); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("review decision from wrong origin accepted: %v", err)
	}
	decision.Header().Set("Origin", origin)
	approved, err := w.DecideReview(ctx, decision)
	if err != nil || approved.Msg.Decision.Action != "approve" {
		t.Fatalf("owner review decision: %+v, %v", approved, err)
	}
	if got, err := w.DecideReview(ctx, decision); err != nil || got.Msg.Decision.Id != approved.Msg.Decision.Id {
		t.Fatalf("decision replay: %+v, %v", got, err)
	}
	recordedCheckpoint, err := w.RecordGoalCheckpoint(ctx, checkpoint)
	if err != nil || !recordedCheckpoint.Msg.Checkpoint.CurrentAcceptance || recordedCheckpoint.Msg.Checkpoint.PlanVersion != 1 || recordedCheckpoint.Msg.Checkpoint.CandidateRevision != strings.Repeat("d", 40) {
		t.Fatalf("record accepted checkpoint: %+v %v", recordedCheckpoint, err)
	}
	if replay, err := w.RecordGoalCheckpoint(ctx, checkpoint); err != nil || replay.Msg.Checkpoint.Id != recordedCheckpoint.Msg.Checkpoint.Id {
		t.Fatalf("checkpoint retry duplicated: %v", err)
	}
	checkpoint.Msg.Title = "Changed title"
	if _, err := w.RecordGoalCheckpoint(ctx, checkpoint); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("changed checkpoint retry: %v", err)
	}
	checkpoint.Msg.Title = "Behavior preserved"
	if _, err := pool.Exec(ctx, `UPDATE workflow_goal_checkpoints SET title='changed' WHERE organization_id=$1 AND id=$2`, owner.OrganizationID, recordedCheckpoint.Msg.Checkpoint.Id); err == nil {
		t.Fatal("checkpoint history changed")
	}
	seedAPIReviewProof(t, pool, owner.OrganizationID, run.ID, strings.Repeat("f", 40))
	nextReview, err := store.PresentForReview(ctx, owner.OrganizationID, run.ID, strings.Repeat("f", 40), currentReview.EvidenceSHA256, run.VerificationSHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := w.GetCurrentReview(ctx, getReview); err != nil || got.Msg.Package.Id != nextReview.ID || got.Msg.Package.Decision != nil {
		t.Fatalf("superseded approval stayed current: %+v, %v", got, err)
	}
	if _, err := w.DecideReview(ctx, decision); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("old package was approved again: %v", err)
	}
	changes := connect.NewRequest(&api.DecideReviewRequest{RunId: run.ID, PackageId: nextReview.ID,
		IdempotencyKey: "browser-changes", Action: "request_changes"})
	changes.Header().Set("Origin", origin)
	changes.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.DecideReview(ctx, changes); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty correction feedback accepted: %v", err)
	}
	changes.Msg.Feedback = "Please fix the failed verification and rerun the checks."
	if got, err := w.DecideReview(ctx, changes); err != nil || got.Msg.Decision.Action != "request_changes" || got.Msg.Decision.Feedback != changes.Msg.Feedback {
		t.Fatalf("owner requested changes: %+v, %v", got, err)
	}
	if got, err := w.GetCurrentReview(ctx, getReview); err != nil || got.Msg.Package.Decision.Action != "request_changes" || got.Msg.Package.Decision.Feedback != changes.Msg.Feedback {
		t.Fatalf("review decision not visible: %+v, %v", got, err)
	}
	changes.Msg.Feedback = "Please change a different thing instead."
	if _, err := w.DecideReview(ctx, changes); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("replayed correction changed feedback: %v", err)
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
	otherTokens, err := manager.LoginLocal(ctx, "another-org", "alice@example.com", password, netip.MustParseAddr("192.0.2.55"), "")
	if err != nil {
		t.Fatal(err)
	}
	other := apiv1connect.NewWorkflowServiceClient(&http.Client{Timeout: 3 * time.Second, Transport: client.Transport}, origin+"/api")
	cookie := "__Host-blaxsmith_access=" + otherTokens.Access
	otherStream, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	otherStream.Header.Set("Origin", origin)
	otherStream.Header.Set("Cookie", cookie)
	otherStream.Header.Set("Last-Event-ID", "2")
	otherResponse, err := (&http.Client{Timeout: 3 * time.Second, Transport: client.Transport}).Do(otherStream)
	if err != nil {
		t.Fatal(err)
	}
	otherResponse.Body.Close()
	if otherResponse.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-tenant event stream: HTTP %d", otherResponse.StatusCode)
	}
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
	runTasks.Header().Set("Cookie", cookie)
	if _, err := other.ListRunTasks(ctx, runTasks); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant run graph: %v", err)
	}
	listRuns.Header().Set("Cookie", cookie)
	if _, err := other.ListRuns(ctx, listRuns); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant run list: %v", err)
	}
	events.Header().Set("Cookie", cookie)
	if _, err := other.EventsAfter(ctx, events); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant events: %v", err)
	}
	exits.Header().Set("Cookie", cookie)
	if _, err := other.ListCommandExits(ctx, exits); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant command exits: %v", err)
	}
	getReview.Header().Set("Cookie", cookie)
	if _, err := other.GetCurrentReview(ctx, getReview); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant review lookup: %v", err)
	}
	decision.Msg.PackageId = nextReview.ID
	decision.Msg.IdempotencyKey = "cross-tenant-approve"
	decision.Header().Set("Cookie", cookie+"; __Host-blaxsmith_csrf="+csrf)
	if _, err := other.DecideReview(ctx, decision); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("cross-tenant review decision: %v", err)
	}
	readCheckpoints := connect.NewRequest(&api.ListGoalCheckpointsRequest{GoalId: createdGoal.Msg.Goal.Id})
	readCheckpoints.Header().Set("Origin", origin)
	historical, err := w.ListGoalCheckpoints(ctx, readCheckpoints)
	if err != nil || len(historical.Msg.Checkpoints) != 1 || historical.Msg.Checkpoints[0].CurrentAcceptance || historical.Msg.Checkpoints[0].CandidateRevision != strings.Repeat("d", 40) {
		t.Fatalf("superseded checkpoint disappeared/stayed current: %+v %v", historical, err)
	}
	if replay, err := w.RecordGoalCheckpoint(ctx, checkpoint); err != nil || replay.Msg.Checkpoint.CurrentAcceptance {
		t.Fatalf("historical checkpoint retry lost receipt: %v", err)
	}
	checkpoint.Msg.RequestKey = "new-rejected-checkpoint"
	if _, err := w.RecordGoalCheckpoint(ctx, checkpoint); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("superseded acceptance checkpoint: %v", err)
	}
	testUsageReporting(t, ctx, pool, client, origin, store, attempt)
	testMachineAPI(t, ctx, pool, client, origin, csrf, first.Id, run.ID, owner)
	testMCPOAuth(t, ctx, pool, client, origin, csrf, first.Id)
	if os.Getenv("BLAXSMITH_E2E_BROWSER") == "1" {
		// Catalog-only fixture: no secret, provider call or runtime approval.
		for _, statement := range []string{
			`INSERT INTO access_provider_registrations (organization_id,id,provider_kind,origin,delivery_modes,state) VALUES ($1,'e2e-advice-provider','openai','https://api.openai.com',ARRAY['native_raw'],'active')`,
			`INSERT INTO access_connections (organization_id,id,owner_kind,owner_id,provider_registration_id,external_account_id,auth_method,state,label,models_checked_at) VALUES ($1,'e2e-advice-connection','organization',$1,'e2e-advice-provider','catalog-only','api_key','active','E2E catalog',clock_timestamp())`,
			`INSERT INTO access_connection_models (organization_id,connection_id,model_id,display_name,efforts,default_effort) SELECT $1,'e2e-advice-connection',model,model,ARRAY['low','medium','high'],'medium' FROM unnest(ARRAY['gpt-6-luna','gpt-6-sol','gpt-6-astra']) model`,
		} {
			if _, err := pool.Exec(ctx, statement, owner.OrganizationID); err != nil {
				t.Fatal(err)
			}
		}
		browserCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		browser := exec.CommandContext(browserCtx, "node", "../../frontend/e2e/native-factory.mjs")
		browser.Env = append(os.Environ(), "BLAXSMITH_E2E_ORIGIN="+origin, "BLAXSMITH_E2E_REPORT_RUN="+run.ID, "BLAXSMITH_E2E_REPORT_PROJECT="+first.Id, "BLAXSMITH_E2E_CHECKPOINT_GOAL="+createdGoal.Msg.Goal.Id)
		if output, err := browser.CombinedOutput(); err != nil {
			t.Fatalf("browser E2E: %v\n%s", err, output)
		} else {
			t.Log(string(output))
		}
	}
	interactionID, interactionAttempt := testInteractionBrowserAPI(t, ctx, pool, client, origin, csrf, owner)
	testAdminBrowserAPI(t, ctx, client, origin, csrf, true)
	testWorkspaceBrowserAPI(t, ctx, client, origin, true)
	testExtensionBrowserAPI(t, ctx, client, origin, csrf, true)
	testUsersBrowserAPI(t, ctx, client, origin, csrf, owner.PrincipalID, true)
	testGitHubConnectGate(t, ctx, client, origin, csrf, first.Id, true)
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
	testAdminBrowserAPI(t, ctx, client, origin, csrf, false)
	testWorkspaceBrowserAPI(t, ctx, client, origin, false)
	testExtensionBrowserAPI(t, ctx, client, origin, csrf, false)
	testUsersBrowserAPI(t, ctx, client, origin, csrf, owner.PrincipalID, false)
	testGitHubConnectGate(t, ctx, client, origin, csrf, first.Id, false)
	viewerCreate := connect.NewRequest(&api.CreateProjectRequest{Slug: "viewer-denied", Name: "Denied"})
	viewerCreate.Header().Set("Origin", origin)
	viewerCreate.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.CreateProject(ctx, viewerCreate); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer created project: %v", err)
	}
	viewerDecision := connect.NewRequest(&api.DecideReviewRequest{RunId: run.ID, PackageId: nextReview.ID,
		IdempotencyKey: "viewer-approve", Action: "approve"})
	viewerDecision.Header().Set("Origin", origin)
	viewerDecision.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.DecideReview(ctx, viewerDecision); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer decided final review: %v", err)
	}
	viewerAnswer := connect.NewRequest(&api.AnswerInteractionRequest{InteractionId: interactionID, OptionIds: []string{"a"}})
	viewerAnswer.Header().Set("Origin", origin)
	viewerAnswer.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.AnswerInteraction(ctx, viewerAnswer); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer answered interaction: %v", err)
	}
	viewerSteer := connect.NewRequest(&api.SteerAttemptRequest{AttemptId: interactionAttempt, Kind: "pause"})
	viewerSteer.Header().Set("Origin", origin)
	viewerSteer.Header().Set("X-Blaxsmith-CSRF", csrf)
	if _, err := w.SteerAttempt(ctx, viewerSteer); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("viewer steered attempt: %v", err)
	}
	viewerList := connect.NewRequest(&api.ListInteractionsRequest{RunId: run.ID})
	viewerList.Header().Set("Origin", origin)
	if _, err := w.ListInteractions(ctx, viewerList); err != nil {
		t.Fatalf("viewer list interactions: %v", err)
	}
	head, err := store.EventHead(ctx, owner.OrganizationID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	revokedRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, origin+"/api/runs/"+run.ID+"/events?after="+strconv.FormatInt(head, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	revokedRequest.Header.Set("Origin", origin)
	revokedStream, err := client.Do(revokedRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer revokedStream.Body.Close()
	if revokedStream.StatusCode != http.StatusOK {
		t.Fatalf("pre-revocation stream: HTTP %d", revokedStream.StatusCode)
	}
	secondRevokedRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, secondOrigin+"/api/runs/"+run.ID+"/events?after="+strconv.FormatInt(head, 10), nil)
	if err != nil {
		t.Fatal(err)
	}
	secondRevokedRequest.Host = strings.TrimPrefix(origin, "https://")
	secondRevokedRequest.Header.Set("Origin", origin)
	secondRevokedStream, err := client.Do(secondRevokedRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer secondRevokedStream.Body.Close()
	if secondRevokedStream.StatusCode != http.StatusOK {
		t.Fatalf("second pre-revocation stream: HTTP %d", secondRevokedStream.StatusCode)
	}
	if _, err := pool.Exec(ctx, `UPDATE identity_sessions SET revoked_at=clock_timestamp()
		WHERE organization_id=$1 AND principal_id=$2 AND revoked_at IS NULL`, owner.OrganizationID, owner.PrincipalID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `SELECT pg_notify('blaxsmith_workflow_events',$1)`, owner.OrganizationID+":"+run.ID); err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(revokedStream.Body)
	if err != nil || strings.Contains(string(content), "data:") {
		t.Fatalf("revoked stream kept delivering: %q, %v", content, err)
	}
	content, err = io.ReadAll(secondRevokedStream.Body)
	if err != nil || strings.Contains(string(content), "data:") {
		t.Fatalf("revoked stream on second replica kept delivering: %q, %v", content, err)
	}
	// Revoke exactly after the first 100 replayed records. A stream must
	// recheck its session before reading and sending the next batch.
	fresh, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, netip.MustParseAddr("192.0.2.88"), "")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := manager.ValidateAccess(ctx, fresh.Access)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.EventHead(ctx, owner.OrganizationID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `WITH advanced AS (
		UPDATE workflow_runs SET event_seq=event_seq+101 WHERE organization_id=$1 AND id=$2
		RETURNING event_seq-101 AS previous
	) INSERT INTO workflow_events (id,organization_id,run_id,kind)
	SELECT previous+n,$1,$2,'run.created' FROM advanced,generate_series(1,101) AS n`, owner.OrganizationID, run.ID); err != nil {
		t.Fatal(err)
	}
	guard, err := identity.NewBrowserGuard(manager, origin)
	if err != nil {
		t.Fatal(err)
	}
	replayCtx, cancelReplay := context.WithTimeout(ctx, time.Second)
	defer cancelReplay()
	replayRequest := httptest.NewRequestWithContext(replayCtx, http.MethodGet,
		origin+"/api/runs/"+run.ID+"/events?after="+strconv.FormatInt(before, 10), nil)
	replayRequest.SetPathValue("runID", run.ID)
	replayRequest.Header.Set("Origin", origin)
	replayRequest.Header.Set("Cookie", "__Host-blaxsmith_access="+fresh.Access)
	var revokeErr error
	writer := &revokingActivityWriter{ResponseRecorder: httptest.NewRecorder()}
	writer.onHundredth = func() { revokeErr = manager.Revoke(ctx, caller) }
	(&runActivityHandler{guard: guard, store: store, hub: newActivityHub(pool)}).ServeHTTP(writer, replayRequest)
	if revokeErr != nil || writer.count != 100 {
		t.Fatalf("replay continued after revocation: events=%d revoke=%v", writer.count, revokeErr)
	}

	// Draining: a caught-up stream ends at once with a short retry hint so
	// EventSource reconnects promptly to a replica that is still serving.
	draining, err := manager.LoginLocal(ctx, "engineering", "alice@example.com", password, netip.MustParseAddr("192.0.2.89"), "")
	if err != nil {
		t.Fatal(err)
	}
	head, err = store.EventHead(ctx, owner.OrganizationID, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	drain := newDrainer()
	drain.begin()
	drainCtx, cancelDrain := context.WithTimeout(ctx, 5*time.Second)
	defer cancelDrain()
	drainRequest := httptest.NewRequestWithContext(drainCtx, http.MethodGet,
		origin+"/api/runs/"+run.ID+"/events?after="+strconv.FormatInt(head, 10), nil)
	drainRequest.SetPathValue("runID", run.ID)
	drainRequest.Header.Set("Origin", origin)
	drainRequest.Header.Set("Cookie", "__Host-blaxsmith_access="+draining.Access)
	drained := &revokingActivityWriter{ResponseRecorder: httptest.NewRecorder()}
	(&runActivityHandler{guard: guard, store: store, hub: newActivityHub(pool), closing: drain.done()}).ServeHTTP(drained, drainRequest)
	if drainCtx.Err() != nil || drained.Code != http.StatusOK || !strings.HasSuffix(drained.Body.String(), "retry: 1000\n: server restarting\n\n") {
		t.Fatalf("draining stream did not end with a retry hint: %d %q %v", drained.Code, drained.Body.String(), drainCtx.Err())
	}
}

type revokingActivityWriter struct {
	*httptest.ResponseRecorder
	onHundredth func()
	count       int
}

func (w *revokingActivityWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(p)
	if strings.HasPrefix(string(p), "id: ") {
		w.count++
		if w.count == 100 {
			w.onHundredth()
		}
	}
	return n, err
}

func (w *revokingActivityWriter) SetWriteDeadline(time.Time) error { return nil }

func readRunActivityEvent(t *testing.T, reader *bufio.Reader) struct {
	ID     string `json:"id"`
	RunID  string `json:"runId"`
	TaskID string `json:"taskId"`
	Kind   string `json:"kind"`
} {
	t.Helper()
	var id, data string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && data != "":
			if _, err := strconv.ParseInt(id, 10, 64); err != nil {
				t.Fatal(err)
			}
			var event struct {
				ID     string `json:"id"`
				RunID  string `json:"runId"`
				TaskID string `json:"taskId"`
				Kind   string `json:"kind"`
			}
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				t.Fatal(err)
			}
			if id != event.ID {
				t.Fatalf("SSE id %s differs from event %s", id, event.ID)
			}
			return event
		}
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

const apiReviewPolicy = `{"schema_version":"blaxsmith.verification/v1alpha1","checks":[{"id":"tests","command":["true"]}]}`

func seedAPIReviewProof(t *testing.T, pool *pgxpool.Pool, org, run, revision string) {
	t.Helper()
	ctx := tenant.System(t.Context())
	var task, attempt, policy string
	var generation int
	if err := pool.QueryRow(ctx, `SELECT t.id,t.generation,r.verification_sha256 FROM workflow_tasks t JOIN workflow_runs r ON r.organization_id=t.organization_id AND r.id=t.run_id WHERE t.organization_id=$1 AND t.run_id=$2 ORDER BY t.created_at LIMIT 1`, org, run).Scan(&task, &generation, &policy); err != nil {
		t.Fatal(err)
	}
	generation++
	if err := pool.QueryRow(ctx, `INSERT INTO workflow_attempts(organization_id,run_id,task_id,generation,state) VALUES($1,$2,$3,$4,'succeeded') RETURNING id`, org, run, task, generation).Scan(&attempt); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workflow_tasks SET generation=$3,state='succeeded',active_attempt_id=NULL WHERE organization_id=$1 AND id=$2`, org, task, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workflow_evidence(organization_id,run_id,task_id,attempt_id,kind,origin_key,revision,policy_sha256,metadata,sha256)
 VALUES($1,$2,$3,$4,'verification','tests',$5,$6,'{"check":"tests","exit_code":0,"verdict":"pass"}',$7)`, org, run, task, attempt, revision, policy, fmt.Sprintf("%x", sha256.Sum256(nil))); err != nil {
		t.Fatal(err)
	}
}

func apiReviewBundle() ([]byte, string) {
	b := recipe.Bundle{SchemaVersion: "blaxsmith.bundle/v1alpha1", Source: recipe.Source{Commit: strings.Repeat("a", 40), Scope: "."}, Recipe: recipe.Recipe{Acceptance: "manual", Profiles: map[string]recipe.Profile{"worker": {Harness: "codex", Model: "gpt-6-sol", Effort: "medium"}}, Stages: []recipe.Stage{{ID: "implement", Kind: "implement", Profile: "worker"}}}}
	data, _ := json.Marshal(b)
	b.Digest = fmt.Sprintf("%x", sha256.Sum256(data))
	data, _ = json.Marshal(b)
	return data, b.Digest
}
