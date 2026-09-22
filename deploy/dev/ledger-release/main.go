// Command probe-ledger-release opens a synthetic AX task through the durable
// ledger. It is a dev-node probe, not a credential or product connector.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
)

func main() {
	var space, task, image, pool, routerIP, routerCA, actorCA, signerFile, database string
	var generation int64
	var finish bool
	flag.StringVar(&space, "space", "", "AX atespace")
	flag.StringVar(&task, "task", "", "AX task name")
	flag.StringVar(&image, "image", "", "approved runner image digest")
	flag.StringVar(&pool, "pool", "", "expected worker pool")
	flag.StringVar(&routerIP, "router-ip", "", "router service IP")
	flag.StringVar(&routerCA, "router-ca", "", "router trust bundle PEM")
	flag.StringVar(&actorCA, "actor-ca", "", "actor trust bundle PEM")
	flag.StringVar(&signerFile, "signer", "", "root-owned synthetic Ed25519 seed file")
	flag.StringVar(&database, "database", "host=/var/run/postgresql user=root dbname=blaxsmith_dev sslmode=disable", "dev PostgreSQL connection")
	flag.Int64Var(&generation, "previous-generation", 0, "prior scheduler owner generation")
	flag.BoolVar(&finish, "finish", false, "deactivate the current synthetic owner")
	flag.Parse()
	if finish {
		if err := deactivate(space, task, database, generation); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(space, task, image, pool, routerIP, routerCA, actorCA, signerFile, database, generation); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func deactivate(space, task, database string, generation int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	poolDB, err := pgxpool.New(ctx, database)
	if err != nil {
		return err
	}
	defer poolDB.Close()
	scope, err := bootstrap.NewLedger(poolDB).Deactivate(ctx, bootstrap.Scope{
		ClusterID: "dev-cluster", AttemptID: space + "/" + task, OwnerGeneration: generation,
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"deactivated": true,
		"fenced_generation": scope.OwnerGeneration})
}

func run(space, task, image, pool, routerIP, routerCA, actorCA, signerFile, database string, generation int64) error {
	if space == "" || task == "" || !strings.Contains(image, "@sha256:") || pool == "" ||
		net.ParseIP(routerIP) == nil || routerCA == "" || actorCA == "" || signerFile == "" {
		return errors.New("incomplete synthetic connector inputs")
	}
	keyInfo, err := os.Stat(signerFile)
	if err != nil || keyInfo.Mode().Perm()&0077 != 0 {
		return errors.New("synthetic signer is unavailable or not owner-only")
	}
	seed, err := os.ReadFile(signerFile)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("invalid synthetic signer")
	}
	signer := ed25519.NewKeyFromSeed(seed)
	servicePEM, err := os.ReadFile(routerCA)
	if err != nil {
		return err
	}
	serviceRoots := x509.NewCertPool()
	if !serviceRoots.AppendCertsFromPEM(servicePEM) {
		return errors.New("invalid router CA")
	}
	actorPEM, err := os.ReadFile(actorCA)
	if err != nil {
		return err
	}
	actorRoots := x509.NewCertPool()
	if !actorRoots.AppendCertsFromPEM(actorPEM) {
		return errors.New("invalid actor CA")
	}
	tokenBytes, err := io.ReadAll(io.LimitReader(os.Stdin, 16385))
	if err != nil || len(tokenBytes) > 16384 {
		return errors.New("connector token unavailable")
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return errors.New("connector token unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	poolDB, err := pgxpool.New(ctx, database)
	if err != nil {
		return err
	}
	defer poolDB.Close()
	current := func(ctx context.Context) (bootstrap.Runtime, error) {
		return observe(ctx, space, task)
	}
	expected, err := current(ctx)
	if err != nil {
		return err
	}
	if expected.Image != image || expected.WorkerPool != pool || expected.SandboxClass != "SANDBOX_CLASS_GVISOR" {
		return bootstrap.ErrDenied
	}
	ledger := bootstrap.NewLedger(poolDB)
	scope, err := ledger.Assign(ctx, "dev-cluster", space+"/"+task, generation, expected.Actor)
	if err != nil {
		return err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: serviceRoots, ServerName: "atenet-router.ate-system.svc", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(routerIP, "443"))
		}}
	defer transport.CloseIdleConnections()
	connector := &bootstrap.Connector{Ledger: ledger, Client: &http.Client{Transport: transport, Timeout: 20 * time.Second},
		RouterURL: "https://atenet-router.ate-system.svc", Roots: actorRoots, Signer: signer,
		Token: func(context.Context) (string, error) { return token, nil }, Current: current,
		Authorize: func(_ context.Context, runtime bootstrap.Runtime) error {
			if runtime.Image != image || runtime.WorkerPool != pool || runtime.SandboxClass != "SANDBOX_CLASS_GVISOR" {
				return bootstrap.ErrDenied
			}
			return nil // Synthetic gate only: no credential or repository is issued.
		},
	}
	if err := connector.Open(ctx, scope, expected); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"cluster_id": scope.ClusterID, "attempt_id": scope.AttemptID,
		"owner_generation": scope.OwnerGeneration, "actor_uid": expected.Actor.UID,
		"template_uid": expected.TemplateUID, "worker_pool": expected.WorkerPool,
		"opened": true,
	})
}

func observe(ctx context.Context, space, task string) (bootstrap.Runtime, error) {
	read := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "kubectl-ate", args...)
		output, err := cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("Substrate read failed: %w", err)
		}
		return output, nil
	}
	actorJSON, err := read("get", "actor", task, "-a", space, "-o", "json")
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	var actors struct {
		Actors []struct {
			Metadata      struct{ Atespace, Name, UID string }
			ActorTemplate struct{ Atespace, Name string }
			Status        struct {
				State, CurrentActorTemplateUID string
				WorkerAssignment               struct{ WorkerPod, WorkerPodUID, WorkerPool string }
			}
		}
	}
	if err := json.Unmarshal(actorJSON, &actors); err != nil || len(actors.Actors) != 1 {
		return bootstrap.Runtime{}, bootstrap.ErrDenied
	}
	a := actors.Actors[0]
	if a.Status.State != "ACTOR_STATE_RUNNING" || a.Metadata.Atespace != space || a.Metadata.Name != task {
		return bootstrap.Runtime{}, bootstrap.ErrDenied
	}
	templateJSON, err := read("get", "actor-template", a.ActorTemplate.Name, "-a", a.ActorTemplate.Atespace, "-o", "json")
	if err != nil {
		return bootstrap.Runtime{}, err
	}
	var templates struct {
		ActorTemplates []struct {
			Metadata   struct{ UID string }
			Containers []struct {
				Image   string
				Command []string
				Env     []struct{ Name, Value string }
			}
			SandboxConfig struct{ SandboxClass string }
		}
	}
	if err := json.Unmarshal(templateJSON, &templates); err != nil || len(templates.ActorTemplates) != 1 {
		return bootstrap.Runtime{}, bootstrap.ErrDenied
	}
	tmpl := templates.ActorTemplates[0]
	if a.Status.CurrentActorTemplateUID != tmpl.Metadata.UID || len(tmpl.Containers) != 1 ||
		len(tmpl.Containers[0].Command) != 1 || tmpl.Containers[0].Command[0] != "/usr/local/bin/ax-task-runner" {
		return bootstrap.Runtime{}, bootstrap.ErrDenied
	}
	var publicKey string
	for _, env := range tmpl.Containers[0].Env {
		if env.Name == "BLAXSMITH_BOOTSTRAP_PUBLIC_KEY" {
			if publicKey != "" {
				return bootstrap.Runtime{}, bootstrap.ErrDenied
			}
			publicKey = env.Value
		}
	}
	return bootstrap.Runtime{Actor: bootstrap.Actor{Atespace: a.Metadata.Atespace, Name: a.Metadata.Name, UID: a.Metadata.UID},
		TemplateUID: tmpl.Metadata.UID, Image: tmpl.Containers[0].Image,
		SandboxClass: tmpl.SandboxConfig.SandboxClass, BootstrapPublicKey: publicKey,
		WorkerPod: a.Status.WorkerAssignment.WorkerPod, WorkerPodUID: a.Status.WorkerAssignment.WorkerPodUID,
		WorkerPool: a.Status.WorkerAssignment.WorkerPool}, nil
}
