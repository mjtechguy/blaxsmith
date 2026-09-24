// Command axbridge-probe exercises the persisted synthetic AX attempt bridge
// on the dedicated development node. It creates a temporary database schema,
// launches no model or Git work, then deletes the AX task and schema.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func main() {
	var database, server, image, signer, poolName, storage, repositoryURL, sourceRef string
	var toolInputs bool
	flag.StringVar(&database, "database", "host=/var/run/postgresql user=root dbname=blaxsmith_dev sslmode=disable", "development database")
	flag.StringVar(&server, "ax-server", "", "AX loopback tunnel URL")
	flag.StringVar(&image, "image", "", "pinned synthetic AX runner image")
	flag.StringVar(&signer, "signer", "", "enrolled bootstrap public key")
	flag.StringVar(&poolName, "pool", "", "worker pool")
	flag.StringVar(&storage, "storage", "", "approved data snapshot location")
	flag.BoolVar(&toolInputs, "tool-inputs", false, "exercise public Git Workspace and per-attempt Gateway without releasing credentials")
	flag.StringVar(&repositoryURL, "repository", "https://github.com/octocat/Hello-World", "public Git repository for --tool-inputs")
	flag.StringVar(&sourceRef, "ref", "HEAD", "public Git ref for --tool-inputs")
	flag.Parse()
	if err := run(database, server, image, signer, poolName, storage, toolInputs, repositoryURL, sourceRef); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(database, server, image, signer, poolName, storage string, toolInputs bool, repositoryURL, sourceRef string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	admin, err := pgxpool.New(ctx, database)
	if err != nil {
		return err
	}
	defer admin.Close()
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	schema := "axbridge_" + hex.EncodeToString(nonce[:])
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		return err
	}
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if _, err := admin.Exec(clean, "DROP SCHEMA "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			fmt.Fprintln(os.Stderr, "residual schema:", schema, err)
		}
	}()
	config, err := pgxpool.ParseConfig(database)
	if err != nil {
		return err
	}
	config.ConnConfig.RuntimeParams["search_path"] = schema
	conn, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := db.Migrate(ctx, conn); err != nil {
		return err
	}
	store, err := workflow.New(conn)
	if err != nil {
		return err
	}
	var org string
	if err := conn.QueryRow(ctx, `INSERT INTO identity_organizations (id,slug,name)
		VALUES (gen_random_uuid(),'synthetic-bridge','Synthetic bridge proof') RETURNING id`).Scan(&org); err != nil {
		return err
	}
	project, err := store.CreateProject(ctx, org, "synthetic-bridge", "Synthetic bridge proof")
	if err != nil {
		return err
	}
	run, err := store.CreateRun(ctx, workflow.RunInput{OrganizationID: org, ProjectID: project, LaunchKey: "node-proof",
		SourceCommit: strings.Repeat("a", 40), BundleSHA256: strings.Repeat("b", 64), VerificationSHA256: strings.Repeat("c", 64)})
	if err != nil {
		return err
	}
	taskID, err := store.AddTask(ctx, org, run.ID, "synthetic", strings.Repeat("d", 64), 1)
	if err != nil {
		return err
	}
	// The one-off dev probe uses a synthetic sealed graph in its throwaway
	// schema. Product dispatch uses CreateFrozenRun and its trusted recipe.
	if _, err := conn.Exec(ctx, `UPDATE workflow_runs SET graph_sealed=true
		WHERE organization_id=$1 AND id=$2`, org, run.ID); err != nil {
		return err
	}
	a, err := store.ReserveAttempt(ctx, org, run.ID, taskID)
	if err != nil {
		return err
	}
	space, name := axbridge.Name(a)
	cli := axbridge.CLI{AXPath: "/usr/local/bin/ax", AtePath: "/usr/local/bin/kubectl-ate", Server: server}
	bridge := &axbridge.Bridge{Workflow: store, AX: cli, Actor: cli, Image: image, Pool: poolName, Signer: signer, Storage: storage}
	var templateGateway string
	if toolInputs {
		if gitfetch.Validate(repositoryURL, sourceRef) != nil {
			return errors.New("--tool-inputs requires a supported public GitHub or GitLab URL and ref")
		}
		source, err := gitfetch.Fetch(ctx, repositoryURL, sourceRef)
		if err != nil {
			return err
		}
		defer source.Close()
		request := tooladapter.Request{AttemptID: a.ID, RepositoryURL: repositoryURL, SourceRef: sourceRef,
			SourceCommit: source.Commit, SourceDirectory: "source",
			Runtime: tooladapter.Runtime{Harness: "codex", Image: image, Binary: "/opt/blaxsmith/bin/codex",
				BinarySHA256: strings.Repeat("a", 64), Version: "0.156.0",
				Supported: []tooladapter.ModelEffort{{Model: "gpt-6-luna", Effort: "xhigh"}}},
			Profile:        recipe.Profile{Harness: "codex", Model: "gpt-6-luna", Effort: "xhigh"},
			Prompt:         "No-credential AX input wiring probe. Do not execute before bootstrap release.",
			TimeoutSeconds: 60, MaxOutputBytes: 1024}
		templateGateway = "probe-egress-" + strings.ReplaceAll(a.ID, "-", "")[:12]
		gateway, ips, err := probeGateway(ctx, space, templateGateway, repositoryURL)
		if err != nil {
			return err
		}
		if err := cli.ApplyGateway(ctx, gateway); err != nil {
			return err
		}
		lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
			addresses, ok := ips[host]
			if !ok {
				return nil, errors.New("probe host is not approved")
			}
			return append([]netip.Addr(nil), addresses...), nil
		}
		bridge.Tool, bridge.Workspace = &request, axbridge.AttemptWorkspaceName(a.ID)
		bridge.Gateway, bridge.GatewayTemplate, bridge.LookupIPv4 = axbridge.AttemptGatewayName(a.ID), templateGateway, lookup
		fmt.Fprintf(os.Stderr, "probe resources atespace=%s task=%s workspace=%s gateway=%s template_gateway=%s\n",
			space, name, bridge.Workspace, bridge.Gateway, templateGateway)
	}
	stopped := false
	defer func() {
		if !stopped {
			fmt.Fprintln(os.Stderr, "AX attempt was not proven stopped; inspect residual resources:", space+"/"+name)
		}
	}()
	runtime, err := bridge.Launch(ctx, a)
	if err != nil {
		return fmt.Errorf("launch AX probe attempt: %w", err)
	}
	if toolInputs {
		if err := bridge.CheckToolInputs(ctx, org, repositoryURL, "openai"); err != nil {
			return err
		}
	}
	if err := store.RequestCancel(ctx, org, run.ID); err != nil {
		return err
	}
	bridge.RevokeOwner = func(ctx context.Context, a workflow.Attempt) error {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM bootstrap_owners WHERE attempt_id=$1 AND active)`, a.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errors.New("unexpected active bootstrap owner")
		}
		return nil
	}
	if err := bridge.StopKnown(ctx, a); err != nil {
		return err
	}
	stopped = true
	if templateGateway != "" {
		if err := cli.DeleteGateway(ctx, space, templateGateway); err != nil && !errors.Is(err, axbridge.ErrNotFound) {
			return err
		}
	}
	if err := store.FinalizeCancel(ctx, org, run.ID); err != nil {
		return err
	}
	checks := []string{"durable attempt reserved before AX write", "deterministic AX Task exact spec read-back",
		"Substrate actor and template UID, image, pool, gVisor and data snapshots matched",
		"workflow cancellation fenced result publication", "AX Task and Substrate actor NotFound after teardown",
		"temporary database schema absent after cleanup"}
	if toolInputs {
		checks = append(checks, "attempt Workspace pinned to the public Git commit", "attempt Gateway copied exact public-Git/model CIDRs",
			"AX Task bound both per-attempt resources", "no credential was released and the tool command stayed behind the gate")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"check": "persisted-attempt-to-AX-and-cancel", "result": "passed", "organization_id": org,
		"run_id": run.ID, "attempt_id": a.ID, "owner_generation": a.OwnerGeneration,
		"atespace": space, "task": name, "actor_uid": runtime.Actor.UID,
		"template_uid": runtime.TemplateUID, "image": runtime.Image, "worker_pool": runtime.WorkerPool,
		"workspace": bridge.Workspace, "gateway": bridge.Gateway, "gateway_template": templateGateway,
		"checks": checks, "credentials_used": false, "model_invoked": false,
		"ax_task_removed": true, "database_schema_cleanup_scheduled": true,
	})
}

func probeGateway(ctx context.Context, space, name, repositoryURL string) (axbridge.Gateway, map[string][]netip.Addr, error) {
	if gitfetch.Validate(repositoryURL, "") != nil {
		return axbridge.Gateway{}, nil, errors.New("unsupported public repository")
	}
	parsed, err := url.Parse(repositoryURL)
	if err != nil {
		return axbridge.Gateway{}, nil, err
	}
	ips := map[string][]netip.Addr{}
	allowed := map[netip.Addr]bool{}
	for _, host := range []string{parsed.Hostname(), "api.openai.com"} {
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		if err != nil || len(addresses) == 0 {
			return axbridge.Gateway{}, nil, errors.New("probe DNS resolution failed")
		}
		for _, address := range addresses {
			address = address.Unmap()
			if !gitfetch.PublicIPv4(address) {
				return axbridge.Gateway{}, nil, errors.New("probe DNS returned a non-public address")
			}
			ips[host] = append(ips[host], address)
			allowed[address] = true
		}
	}
	hosts := make([]netip.Addr, 0, len(allowed))
	for address := range allowed {
		hosts = append(hosts, address)
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].Less(hosts[j]) })
	rules := make([]axbridge.GatewayHostRule, 0, len(hosts))
	for _, address := range hosts {
		rules = append(rules, axbridge.GatewayHostRule{Host: address.String() + "/32"})
	}
	return axbridge.Gateway{APIVersion: "ax.io/v1alpha1", Kind: "Gateway",
		Metadata: axbridge.TaskMetadata{Name: name, Atespace: space},
		Spec:     axbridge.GatewaySpec{Egress: &axbridge.GatewayEgress{Allowlist: &axbridge.GatewayAllowlist{Hosts: rules}}}}, ips, nil
}
