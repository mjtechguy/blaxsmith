package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1/apiv1connect"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/interact"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/terminal"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type appConfig struct {
	listen, origin, certFile, keyFile, signerFile, previousSignerFile, databaseURL, staticDir, migrations string
	allowLocalDatabase                                                                                    bool
	enableDispatch                                                                                        bool
	allowOpenEgressDev                                                                                    bool
	dispatchConfig                                                                                        dispatchConfig
	sessionPolicy                                                                                         identity.SessionPolicy
	shutdownTimeout                                                                                       time.Duration
}

func serveApp(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveAppContext(ctx, args)
}

func serveAppContext(ctx context.Context, args []string) error {
	// Startup and the background loops started here are cross-organization.
	// Request handlers get fresh contexts from net/http, so a request reaches
	// organization rows only through store calls that scope to its caller.
	ctx = tenant.System(ctx)
	config, err := parseAppConfig(args)
	if err != nil {
		return err
	}
	keyInfo, err := os.Stat(config.keyFile)
	if err != nil || !keyInfo.Mode().IsRegular() || keyInfo.Mode().Perm()&0o027 != 0 {
		return errors.New("HTTPS private key must be a regular file without group write or world access")
	}
	certificate, err := tls.LoadX509KeyPair(config.certFile, config.keyFile)
	if err != nil {
		return fmt.Errorf("load HTTPS certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse HTTPS certificate: %w", err)
	}
	origin, _ := url.Parse(config.origin)
	if err := leaf.VerifyHostname(origin.Hostname()); err != nil || time.Now().Before(leaf.NotBefore) || time.Now().After(leaf.NotAfter) {
		return errors.New("HTTPS certificate must be valid for the public origin")
	}
	signer, err := identity.LoadSessionSigner(config.signerFile)
	if err != nil {
		return err
	}
	var previous []ed25519.PublicKey
	if config.previousSignerFile != "" {
		key, err := identity.LoadSessionPublicKey(config.previousSignerFile)
		if err != nil {
			return err
		}
		previous = append(previous, key)
	}
	dbConfig, err := pgxpool.ParseConfig(config.databaseURL)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	if err := validateDatabaseTransport(dbConfig, config.allowLocalDatabase); err != nil {
		return err
	}
	startupCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(startupCtx, tenant.Configure(dbConfig))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(startupCtx); err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	var migrationErr error
	if config.migrations == "verify" {
		migrationErr = db.Verify(startupCtx, pool)
	} else {
		_, migrationErr = db.Migrate(startupCtx, pool)
	}
	if migrationErr != nil {
		return fmt.Errorf("verify database migrations: %w", migrationErr)
	}
	var bypassRLS bool
	if err := pool.QueryRow(startupCtx, `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user`).
		Scan(&bypassRLS); err == nil && bypassRLS {
		log.Printf("database role bypasses row-level security; tenant isolation relies on query filters alone")
	}
	manager, err := identity.NewSessionManager(pool, config.origin, signer, previous...)
	if err != nil {
		return err
	}
	if err := manager.SetPolicy(config.sessionPolicy); err != nil {
		return err
	}
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	activity := newActivityHub(pool)
	listenerDone := make(chan struct{})
	go func() { defer close(listenerDone); activity.listen(serveCtx) }()
	defer func() { stopServing(); <-listenerDone }()
	select {
	case <-activity.ready:
	case <-time.After(5 * time.Second):
		return errors.New("run activity listener unavailable")
	case <-ctx.Done():
		return ctx.Err()
	}
	interactions, err := interact.New(pool)
	if err != nil {
		return err
	}
	// ponytail: the guest router connection lives for the process.
	guests, err := appGuestRouter()
	if err != nil {
		return err
	}
	drain := newDrainer()
	handler, product, err := newAppHandler(startupCtx, pool, manager, config.origin, config.staticDir, activity,
		config.dispatchConfig, interactions, guests, drain)
	if err != nil {
		return err
	}
	if product != nil {
		defer product.Close()
	}
	limits, err := identity.NewLoginLimit(pool)
	if err != nil {
		return err
	}
	if err := limits.Prune(startupCtx); err != nil {
		return fmt.Errorf("initialize login limits: %w", err)
	}
	listener, err := net.Listen("tcp", config.listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 15 * time.Second, WriteTimeout: 180 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}}
	shutdownDone := make(chan error, 1)
	go func() {
		<-serveCtx.Done()
		// SIGTERM: fail readiness and send streams away, then stop accepting
		// and let in-flight RPCs finish. In Kubernetes the pod's preStop sleep
		// has already let endpoints drop this pod before SIGTERM arrives.
		shutdownDone <- drain.shutdown(serveCtx, server, config.shutdownTimeout)
	}()
	pruneDone := make(chan struct{})
	go func() {
		defer close(pruneDone)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-serveCtx.Done():
				return
			case <-ticker.C:
				if err := limits.Prune(serveCtx); err != nil {
					log.Printf("prune login limits: %v", err)
				}
			}
		}
	}()
	dispatchDone := make(chan struct{})
	if product != nil {
		go func() {
			defer close(dispatchDone)
			runDispatchCoordinator(serveCtx, pool, product)
		}()
	} else {
		close(dispatchDone)
	}
	// Moves legacy master-key secrets onto per-organization data keys.
	// Idempotent and batched, so every replica may run it.
	secretsDone := make(chan struct{})
	go func() {
		defer close(secretsDone)
		secrets, err := appSecretStore(pool)
		if err != nil || secrets == nil {
			return
		}
		summary, err := upgradeSecrets(serveCtx, secrets)
		if err != nil {
			log.Printf("upgrade access secrets: %v", err)
			return
		}
		log.Print(summary)
	}()
	interactionDone := make(chan struct{})
	go func() {
		defer close(interactionDone)
		runInteractionCoordinator(serveCtx, pool, interactionStore(pool), interactionWatcher(interactions, guests))
	}()
	fmt.Fprintf(os.Stderr, "Blaxsmith HTTPS API on %s\n", listener.Addr())
	err = server.ServeTLS(listener, "", "")
	stopServing()
	shutdownErr := <-shutdownDone
	<-pruneDone
	<-dispatchDone
	<-interactionDone
	<-secretsDone
	if errors.Is(err, http.ErrServerClosed) {
		return shutdownErr
	}
	return err
}

func parseAppConfig(args []string) (appConfig, error) {
	var c appConfig
	flags := flag.NewFlagSet("serve-app", flag.ContinueOnError)
	flags.StringVar(&c.listen, "listen", "", "direct HTTPS listen address")
	flags.StringVar(&c.origin, "origin", "", "exact public HTTPS origin")
	flags.StringVar(&c.certFile, "tls-cert-file", "", "HTTPS certificate file")
	flags.StringVar(&c.keyFile, "tls-key-file", "", "HTTPS private-key file")
	flags.StringVar(&c.signerFile, "signer-file", "", "platform-mounted 32-byte Ed25519 seed file")
	flags.StringVar(&c.previousSignerFile, "previous-signer-public-file", "", "optional prior 32-byte Ed25519 public key for access-token rotation overlap")
	flags.StringVar(&c.staticDir, "static-dir", "", "built frontend directory containing index.html")
	flags.StringVar(&c.migrations, "migrations", "apply", "apply or verify embedded database migrations before serving")
	flags.BoolVar(&c.allowLocalDatabase, "allow-insecure-local-database", false, "allow plaintext PostgreSQL only over a literal loopback address or Unix socket")
	flags.BoolVar(&c.enableDispatch, "enable-dispatch", false, "enable AX run dispatch after validating pinned runtime and credential configuration")
	flags.BoolVar(&c.allowOpenEgressDev, "allow-open-egress-dev", false, "permit an explicitly configured open AX Gateway for development")
	flags.DurationVar(&c.sessionPolicy.Idle, "session-idle-timeout", identity.DefaultSessionPolicy.Idle,
		"browser sessions end after this long without a refresh (sliding)")
	flags.DurationVar(&c.sessionPolicy.Absolute, "session-absolute-lifetime", identity.DefaultSessionPolicy.Absolute,
		"browser sessions end this long after sign-in regardless of activity")
	flags.DurationVar(&c.shutdownTimeout, "shutdown-timeout", 20*time.Second, "how long in-flight requests may finish after SIGTERM")
	if err := flags.Parse(args); err != nil {
		return c, err
	}
	c.databaseURL = os.Getenv("BLAXSMITH_DATABASE_URL")
	u, err := url.Parse(c.origin)
	if flags.NArg() != 0 || (c.migrations != "apply" && c.migrations != "verify") || c.listen == "" || c.certFile == "" || c.keyFile == "" || c.signerFile == "" || c.databaseURL == "" ||
		err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != c.origin {
		return c, errors.New("serve-app requires --listen, exact HTTPS --origin, --tls-cert-file, --tls-key-file, --signer-file, and BLAXSMITH_DATABASE_URL")
	}
	if err := c.sessionPolicy.Validate(); err != nil {
		return c, err
	}
	if c.shutdownTimeout <= 0 || c.shutdownTimeout > 10*time.Minute {
		return c, errors.New("--shutdown-timeout must be positive and at most 10m")
	}
	c.dispatchConfig, err = parseDispatchConfig(c.enableDispatch, c.allowOpenEgressDev, os.LookupEnv)
	if err != nil {
		return c, err
	}
	return c, nil
}

func validateDatabaseTransport(config *pgxpool.Config, allowLocal bool) error {
	if config == nil {
		return errors.New("database configuration required")
	}
	secure := func(host string, tlsConfig *tls.Config) bool {
		if tlsConfig != nil && !tlsConfig.InsecureSkipVerify {
			return true
		}
		if !allowLocal {
			return false
		}
		if filepath.IsAbs(host) {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	}
	if !secure(config.ConnConfig.Host, config.ConnConfig.TLSConfig) {
		return errors.New("database requires verified TLS; local plaintext requires --allow-insecure-local-database")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !secure(fallback.Host, fallback.TLSConfig) {
			return errors.New("database fallback requires verified TLS or explicit local plaintext")
		}
	}
	return nil
}

func newAppHandler(ctx context.Context, pool *pgxpool.Pool, manager *identity.SessionManager,
	origin, staticDir string, activity *activityHub, dispatchConfig dispatchConfig, interactions *interact.Store,
	guests *terminal.Router, drain *drainer) (http.Handler, *productDispatch, error) {
	authPath, authHandler, err := identity.NewBrowserHandler(manager, origin)
	if err != nil {
		return nil, nil, err
	}
	guard, err := identity.NewBrowserGuard(manager, origin)
	if err != nil {
		return nil, nil, err
	}
	store, err := workflow.New(pool)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := appSecretStore(pool)
	if err != nil {
		return nil, nil, err
	}
	var product *productDispatch
	if dispatchConfig.axServer != "" {
		product, err = newProductDispatch(ctx, dispatchConfig, pool, store, secrets, guests)
		if err != nil {
			return nil, nil, fmt.Errorf("initialize run dispatcher: %w", err)
		}
		if interactions != nil {
			product.Escalations = escalationSink{interactions}
			interactions.OnEscalationAnswer(escalationAnswer(store))
		}
	}
	terminals := terminal.NewHub()
	mux := http.NewServeMux()
	mux.Handle("/api"+authPath, http.StripPrefix("/api", authHandler))
	service := &workflowService{guard: guard, store: store, secrets: secrets, interactions: interactions,
		guests: guests, terminals: terminals}
	if product != nil {
		service.dispatcher = product.Dispatcher
		service.dispatchReady = product.Preflight
		service.launchEnabled = true
	}
	workflowPath, workflowHandler := apiv1connect.NewWorkflowServiceHandler(service, connect.WithReadMaxBytes(1<<20))
	mux.Handle("/api"+workflowPath, http.StripPrefix("/api", guard.Wrap(workflowHandler)))
	admin := newAdminService(guard, store, dispatchConfig.workerPool)
	admin.sessions = manager.Policy()
	adminPath, adminHandler := apiv1connect.NewAdminServiceHandler(admin,
		connect.WithReadMaxBytes(1<<16))
	mux.Handle("/api"+adminPath, http.StripPrefix("/api", guard.Wrap(adminHandler)))
	tools := &catalogService{client: &http.Client{Timeout: 30 * time.Second}}
	connections := newConnectionService(guard, store, secrets, origin)
	connections.tools = tools
	// ponytail: ctx is the startup context; the refresh loop lives for the process.
	go connections.refreshModelsDaily(context.WithoutCancel(ctx))
	connectionPath, connectionHandler := apiv1connect.NewConnectionServiceHandler(connections, connect.WithReadMaxBytes(1<<16))
	mux.Handle("/api"+connectionPath, http.StripPrefix("/api", guard.Wrap(connectionHandler)))
	mux.Handle(githubCallbackPath, guard.Wrap(connections))
	users, err := identity.NewUserAdmin(pool)
	if err != nil {
		return nil, nil, err
	}
	usersPath, usersHandler := apiv1connect.NewUserAdminServiceHandler(&userAdminService{guard: guard, users: users},
		connect.WithReadMaxBytes(1<<14))
	mux.Handle("/api"+usersPath, http.StripPrefix("/api", guard.Wrap(usersHandler)))
	accountPath, accountHandler := apiv1connect.NewAccountServiceHandler(&accountService{guard: guard, manager: manager},
		connect.WithReadMaxBytes(1<<14))
	mux.Handle("/api"+accountPath, http.StripPrefix("/api", guard.Wrap(accountHandler)))
	workspacePath, workspaceHandler := apiv1connect.NewWorkspaceServiceHandler(&workspaceService{guard: guard, store: store, users: users},
		connect.WithReadMaxBytes(1<<14))
	mux.Handle("/api"+workspacePath, http.StripPrefix("/api", guard.Wrap(workspaceHandler)))
	recipePath, recipeHandler := apiv1connect.NewRecipeServiceHandler(&recipeService{guard: guard, store: store, workflow: service},
		connect.WithReadMaxBytes(2<<20))
	mux.Handle("/api"+recipePath, http.StripPrefix("/api", guard.Wrap(recipeHandler)))
	setupPath, setupHandler := apiv1connect.NewSetupServiceHandler(&setupService{guard: guard, store: store, workflow: service},
		connect.WithReadMaxBytes(1<<14))
	mux.Handle("/api"+setupPath, http.StripPrefix("/api", guard.Wrap(setupHandler)))
	extensionPath, extensionHandler := apiv1connect.NewExtensionServiceHandler(newExtensionService(guard, store),
		connect.WithReadMaxBytes(256<<10))
	mux.Handle("/api"+extensionPath, http.StripPrefix("/api", guard.Wrap(extensionHandler)))
	gatewayAdminPath, gatewayAdminHandler := apiv1connect.NewGatewayAdminServiceHandler(
		&gatewayAdminService{guard: guard, store: store, installed: appGatewayURL() != "", secrets: secrets}, connect.WithReadMaxBytes(1<<14))
	mux.Handle("/api"+gatewayAdminPath, http.StripPrefix("/api", guard.Wrap(gatewayAdminHandler)))
	usagePath, usageHandler := apiv1connect.NewUsageServiceHandler(&usageService{guard: guard, store: store},
		connect.WithReadMaxBytes(1<<14))
	mux.Handle("/api"+usagePath, http.StripPrefix("/api", guard.Wrap(usageHandler)))
	mux.Handle("/api/runs/{runID}/events", guard.Wrap(&runActivityHandler{guard: guard, store: store, hub: activity, closing: drain.done()}))
	terminalHandler := newTerminalHandler(guard, origin, store, guests, terminals)
	terminalHandler.closing = drain.done()
	mux.Handle("/api/terminal/attempts/{attemptID}", guard.Wrap(terminalHandler))
	catalogPath, catalogHandler := apiv1connect.NewCatalogServiceHandler(tools)
	mux.Handle("/api"+catalogPath, http.StripPrefix("/api", catalogHandler))
	mux.HandleFunc("/api", http.NotFound)
	mux.HandleFunc("/api/", http.NotFound)
	mux.HandleFunc("/livez", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
	})
	// Readiness: fails while draining so no new traffic is routed here, and
	// while PostgreSQL is unreachable. Liveness (/livez) depends on neither.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if drain.draining() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		checkCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(checkCtx); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("ok\n"))
		}
	})
	if staticDir != "" {
		index := filepath.Join(staticDir, "index.html")
		if info, err := os.Stat(index); err != nil || !info.Mode().IsRegular() {
			if product != nil {
				product.Close()
			}
			return nil, nil, errors.New("static directory must contain a regular index.html")
		}
		files := http.FileServer(http.Dir(staticDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if strings.HasPrefix(r.URL.Path, "/setup/") {
				// The path carries a one-time account token.
				w.Header().Set("Referrer-Policy", "no-referrer")
				w.Header().Set("Cache-Control", "no-store")
			}
			if r.URL.Path == "/" || (filepath.Ext(r.URL.Path) == "" && !strings.HasPrefix(r.URL.Path, "/assets/")) {
				http.ServeFile(w, r, index)
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return mux, product, nil
}
