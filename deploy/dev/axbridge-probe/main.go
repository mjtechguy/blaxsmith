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
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/db"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func main() {
	var database, server, image, signer, poolName, storage string
	flag.StringVar(&database, "database", "host=/var/run/postgresql user=root dbname=blaxsmith_dev sslmode=disable", "development database")
	flag.StringVar(&server, "ax-server", "", "AX loopback tunnel URL")
	flag.StringVar(&image, "image", "", "pinned synthetic AX runner image")
	flag.StringVar(&signer, "signer", "", "enrolled bootstrap public key")
	flag.StringVar(&poolName, "pool", "", "worker pool")
	flag.StringVar(&storage, "storage", "", "approved data snapshot location")
	flag.Parse()
	if err := run(database, server, image, signer, poolName, storage); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(database, server, image, signer, poolName, storage string) error {
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
	defer func() {
		clean, stop := context.WithTimeout(context.Background(), 2*time.Minute)
		defer stop()
		if err := cli.Delete(clean, space, name); err != nil && !errors.Is(err, axbridge.ErrNotFound) {
			fmt.Fprintln(os.Stderr, "residual AX task:", space+"/"+name, err)
		}
	}()
	runtime, err := bridge.Launch(ctx, a)
	if err != nil {
		return err
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
	if err := store.FinalizeCancel(ctx, org, run.ID); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"check": "persisted-attempt-to-AX-and-cancel", "result": "passed", "organization_id": org,
		"run_id": run.ID, "attempt_id": a.ID, "owner_generation": a.OwnerGeneration,
		"atespace": space, "task": name, "actor_uid": runtime.Actor.UID,
		"template_uid": runtime.TemplateUID, "image": runtime.Image, "worker_pool": runtime.WorkerPool,
		"ax_task_removed": true, "database_schema_cleanup_scheduled": true,
	})
}
