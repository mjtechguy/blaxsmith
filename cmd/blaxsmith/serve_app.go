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
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

type appConfig struct {
	listen, origin, certFile, keyFile, signerFile, previousSignerFile, databaseURL, staticDir, migrations string
	allowLocalDatabase                                                                                    bool
}

func serveApp(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serveAppContext(ctx, args)
}

func serveAppContext(ctx context.Context, args []string) error {
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
	pool, err := pgxpool.NewWithConfig(startupCtx, dbConfig)
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
	manager, err := identity.NewSessionManager(pool, config.origin, signer, previous...)
	if err != nil {
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
	handler, err := newAppHandler(pool, manager, config.origin, config.staticDir, activity)
	if err != nil {
		return err
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
		shutdownCtx, stop := context.WithTimeout(context.WithoutCancel(serveCtx), 10*time.Second)
		defer stop()
		shutdownDone <- server.Shutdown(shutdownCtx)
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
	fmt.Fprintf(os.Stderr, "Blaxsmith HTTPS API on %s\n", listener.Addr())
	err = server.ServeTLS(listener, "", "")
	stopServing()
	shutdownErr := <-shutdownDone
	<-pruneDone
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
	if err := flags.Parse(args); err != nil {
		return c, err
	}
	c.databaseURL = os.Getenv("BLAXSMITH_DATABASE_URL")
	u, err := url.Parse(c.origin)
	if flags.NArg() != 0 || (c.migrations != "apply" && c.migrations != "verify") || c.listen == "" || c.certFile == "" || c.keyFile == "" || c.signerFile == "" || c.databaseURL == "" ||
		err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.String() != c.origin {
		return c, errors.New("serve-app requires --listen, exact HTTPS --origin, --tls-cert-file, --tls-key-file, --signer-file, and BLAXSMITH_DATABASE_URL")
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

func newAppHandler(pool *pgxpool.Pool, manager *identity.SessionManager, origin, staticDir string, activity *activityHub) (http.Handler, error) {
	authPath, authHandler, err := identity.NewBrowserHandler(manager, origin)
	if err != nil {
		return nil, err
	}
	guard, err := identity.NewBrowserGuard(manager, origin)
	if err != nil {
		return nil, err
	}
	store, err := workflow.New(pool)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("/api"+authPath, http.StripPrefix("/api", authHandler))
	workflowPath, workflowHandler := apiv1connect.NewWorkflowServiceHandler(&workflowService{
		guard: guard, store: store, launchEnabled: os.Getenv("BLAXSMITH_RUN_LAUNCH_ENABLED") == "1",
	}, connect.WithReadMaxBytes(1<<20))
	mux.Handle("/api"+workflowPath, http.StripPrefix("/api", guard.Wrap(workflowHandler)))
	mux.Handle("/api/runs/{runID}/events", guard.Wrap(&runActivityHandler{guard: guard, store: store, hub: activity}))
	catalogPath, catalogHandler := apiv1connect.NewCatalogServiceHandler(&catalogService{client: &http.Client{Timeout: 30 * time.Second}})
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
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
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
			return nil, errors.New("static directory must contain a regular index.html")
		}
		files := http.FileServer(http.Dir(staticDir))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if r.URL.Path == "/" || (filepath.Ext(r.URL.Path) == "" && !strings.HasPrefix(r.URL.Path, "/assets/")) {
				http.ServeFile(w, r, index)
				return
			}
			files.ServeHTTP(w, r)
		})
	}
	return mux, nil
}
