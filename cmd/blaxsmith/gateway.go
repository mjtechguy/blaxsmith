package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/gateway"
	"github.com/mjtechguy/blaxsmith/internal/tenant"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

// appGatewayURL is the model gateway origin sandboxes call, set by the Helm
// chart only when the gateway Deployment exists (§15.1 installation level).
func appGatewayURL() string {
	value := strings.TrimSuffix(os.Getenv("BLAXSMITH_GATEWAY_URL"), "/")
	if !gateway.ValidBaseURL(value) {
		return ""
	}
	return value
}

type upstreamFlags map[string]string

func (u upstreamFlags) String() string { return "" }
func (u upstreamFlags) Set(value string) error {
	family, target, ok := strings.Cut(value, "=")
	if !ok || !strings.HasPrefix(target, "http://127.0.0.1:") && !strings.HasPrefix(target, "http://localhost:") {
		return errors.New("--dev-upstream takes family=http://127.0.0.1:port (mock probes only)")
	}
	u[family] = strings.TrimSuffix(target, "/")
	return nil
}

// serveGateway runs `blaxsmith gateway`: the brokered model gateway
// (docs/model-gateway-plan.md), its own Deployment sharing PostgreSQL.
func serveGateway(args []string) error {
	flags := flag.NewFlagSet("gateway", flag.ContinueOnError)
	listen := flags.String("listen", ":8443", "listen address")
	certFile := flags.String("tls-cert-file", "", "HTTPS certificate (required unless --allow-plaintext)")
	keyFile := flags.String("tls-key-file", "", "HTTPS private key")
	plaintext := flags.Bool("allow-plaintext", false, "serve plain HTTP (in-cluster behind a TLS proxy, or local probes)")
	localDB := flags.Bool("allow-insecure-local-database", false, "allow plaintext PostgreSQL over loopback")
	tokenRate := flags.Float64("token-rps", 10, "per-token request rate")
	orgRate := flags.Float64("org-rps", 100, "per-organization request rate")
	retention := flags.Duration("event-retention", 90*24*time.Hour, "raw usage event retention for organizations without a gateway setting")
	drain := flags.Duration("drain-timeout", 15*time.Minute, "how long in-flight streams may finish on shutdown")
	upstreams := upstreamFlags{}
	flags.Var(upstreams, "dev-upstream", "family=http://127.0.0.1:port mock upstream (repeatable; probes only)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*certFile == "") != (*keyFile == "") || (*certFile == "" && !*plaintext) ||
		*tokenRate <= 0 || *orgRate <= 0 || *drain <= 0 {
		return errors.New("gateway requires --tls-cert-file/--tls-key-file (or --allow-plaintext) and positive limits")
	}
	if len(upstreams) > 0 && !*plaintext {
		return errors.New("--dev-upstream is for local plaintext probes only")
	}
	dsn := os.Getenv("BLAXSMITH_DATABASE_URL")
	if dsn == "" {
		return errors.New("set BLAXSMITH_DATABASE_URL")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("configure database: %w", err)
	}
	if err := validateDatabaseTransport(config, *localDB); err != nil {
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, tenant.Configure(config))
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	startup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := db.Verify(startup, pool); err != nil { // the app owns migrations.
		return fmt.Errorf("verify database migrations: %w", err)
	}
	secrets, err := appSecretStore(pool)
	if err != nil {
		return err
	}
	if secrets == nil {
		return errors.New("gateway requires BLAXSMITH_ACCESS_KEY_FILE to inject route credentials")
	}
	// The OAuth refresher is the only caller of the Codex token endpoint; the
	// gateway uses it for an owner's own run when the organization serves
	// personal subscription routes (docs/model-gateway-plan.md §6).
	authorizer := &gateway.Authorizer{DB: pool, Secrets: secrets, OAuth: &access.OAuthRefresher{DB: pool, Secrets: secrets}}
	states, shared := gateway.NewStates(), gateway.NewShared(pool)
	proxy := &gateway.Server{DB: pool, Authorize: authorizer.Authorize, Prices: &gateway.PriceBook{DB: pool},
		Upstream: upstreams, TokenRate: *tokenRate, TokenBurst: int(*tokenRate * 2),
		OrgRate: *orgRate, OrgBurst: int(*orgRate * 2), States: states, Shared: shared, RouteKey: authorizer.RouteKey,
		Plan: func(ctx context.Context, g gateway.Grant) (gateway.Plan, error) {
			return gateway.LoadPlan(ctx, pool, g)
		}}
	mux := http.NewServeMux()
	for _, family := range gateway.Families {
		mux.Handle("/"+family+"/", proxy)
	}
	mux.HandleFunc("/livez", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		check, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(check); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	// No write timeout: model streams run for many minutes. Idle and header
	// timeouts still bound slow or abandoned connections.
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10}
	if !*plaintext {
		certificate, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			return fmt.Errorf("load gateway certificate: %w", err)
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}}
	}
	go gatewayMaintenance(ctx, pool, *retention)
	go states.Persist(ctx, pool, 5*time.Second)
	go shared.KeepAlive(ctx, 5*time.Second)
	done := make(chan error, 1)
	go func() {
		<-ctx.Done()
		// Drain: stop accepting, let in-flight streams finish (§15.1).
		shutdown, cancel := context.WithTimeout(context.Background(), *drain)
		defer cancel()
		done <- server.Shutdown(shutdown)
	}()
	fmt.Fprintf(os.Stderr, "Blaxsmith model gateway on %s\n", listener.Addr())
	if *plaintext {
		err = server.Serve(listener)
	} else {
		err = server.ServeTLS(listener, "", "")
	}
	if errors.Is(err, http.ErrServerClosed) {
		return <-done
	}
	return err
}

// gatewayMaintenance keeps partitions ahead, rolls usage up every minute
// (then evaluates soft budgets against the fresh rollups),
// and prunes raw events past retention once a day.
func gatewayMaintenance(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) {
	ctx = tenant.System(ctx)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	var lastPrune time.Time
	for {
		now := time.Now()
		if err := gateway.EnsurePartitions(ctx, pool); err != nil && ctx.Err() == nil {
			log.Printf("gateway partitions: %v", err)
		}
		if err := gateway.Rollup(ctx, pool, now.Add(-24*time.Hour)); err != nil && ctx.Err() == nil {
			log.Printf("gateway rollup: %v", err)
		} else if _, err := workflow.EvaluateBudgets(ctx, pool, now); err != nil && ctx.Err() == nil {
			log.Printf("gateway budgets: %v", err)
		}
		if now.Sub(lastPrune) > 24*time.Hour {
			if err := gateway.PruneEvents(ctx, pool, retention); err != nil && ctx.Err() == nil {
				log.Printf("gateway retention: %v", err)
			}
			lastPrune = now
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
