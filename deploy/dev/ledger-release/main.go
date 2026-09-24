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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
)

func main() {
	var space, task, image, pool, routerIP, routerCA, actorCA, signerFile, database, gitRepo, gitCommit, gitBinding, secretKeyFile string
	var modelBinding, modelProvider, modelName, phase string
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
	flag.StringVar(&gitRepo, "git-repo", "", "synthetic private HTTPS Git repository")
	flag.StringVar(&gitCommit, "git-commit", "", "expected immutable Git commit")
	flag.StringVar(&gitBinding, "git-binding", "", "frozen synthetic Git-read binding")
	flag.StringVar(&secretKeyFile, "secret-key-file", "", "owner-only database encryption key")
	flag.StringVar(&phase, "phase", "setup", "bootstrap phase: setup or model")
	flag.StringVar(&modelBinding, "model-binding", "", "frozen synthetic model-invoke binding")
	flag.StringVar(&modelProvider, "model-provider", "openai", "synthetic model provider")
	flag.StringVar(&modelName, "model", "gpt-6-luna", "synthetic model name")
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
	if err := run(space, task, image, pool, routerIP, routerCA, actorCA, signerFile, database, gitRepo, gitCommit,
		gitBinding, modelBinding, modelProvider, modelName, secretKeyFile, phase, generation); err != nil {
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
	if err := access.RevokeAttemptLeases(ctx, poolDB, scope.ClusterID, scope.AttemptID); err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"deactivated": true,
		"leases_revoked": true, "fenced_generation": scope.OwnerGeneration})
}

func run(space, task, image, pool, routerIP, routerCA, actorCA, signerFile, database, gitRepo, gitCommit,
	gitBinding, modelBinding, modelProvider, modelName, secretKeyFile, phase string, generation int64) error {
	if space == "" || task == "" || !strings.Contains(image, "@sha256:") || pool == "" ||
		net.ParseIP(routerIP) == nil || routerCA == "" || actorCA == "" || signerFile == "" ||
		(gitRepo == "") != (gitBinding == "") || (gitRepo == "") != (gitCommit == "") ||
		((gitRepo != "" || modelBinding != "") != (secretKeyFile != "")) {
		return errors.New("incomplete synthetic connector inputs")
	}
	if (phase != bootstrap.PhaseSetup && phase != bootstrap.PhaseModel) ||
		(phase == bootstrap.PhaseModel && (modelBinding == "" || modelProvider == "" || modelName == "")) {
		return errors.New("incomplete synthetic model phase inputs")
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
	var secretStore *access.SecretStore
	if gitRepo != "" || modelBinding != "" {
		info, err := os.Lstat(secretKeyFile)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return access.ErrDenied
		}
		key, err := os.ReadFile(secretKeyFile)
		if err != nil || len(key) != 32 {
			return access.ErrDenied
		}
		secretStore, err = access.NewSecretStore(poolDB, "dev-key-1", map[string][]byte{"dev-key-1": key})
		clear(key)
		if err != nil {
			return err
		}
	}
	current := func(ctx context.Context) (bootstrap.Runtime, error) {
		return observe(ctx, space, task)
	}
	expected, err := current(ctx)
	if err != nil {
		return err
	}
	if expected.Image != image || expected.WorkerPool != pool || expected.SandboxClass != "SANDBOX_CLASS_GVISOR" || !dataSnapshots(expected) {
		return bootstrap.ErrDenied
	}
	ledger := bootstrap.NewLedger(poolDB)
	var scope bootstrap.Scope
	if phase == bootstrap.PhaseSetup {
		scope, err = ledger.Assign(ctx, "dev-cluster", space+"/"+task, generation, expected.Actor)
	} else {
		var actor bootstrap.Actor
		var active, exists bool
		scope, actor, active, exists, err = ledger.CurrentOwner(ctx, "dev-cluster", space+"/"+task)
		if err == nil && (!exists || !active || actor != expected.Actor || scope.OwnerGeneration != generation) {
			err = bootstrap.ErrDenied
		}
	}
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			_, _ = ledger.Deactivate(cleanupCtx, scope)
			_ = access.RevokeAttemptLeases(cleanupCtx, poolDB, scope.ClusterID, scope.AttemptID)
		}
	}()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: serviceRoots, ServerName: "atenet-router.ate-system.svc", MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp", net.JoinHostPort(routerIP, "443"))
		}}
	defer transport.CloseIdleConnections()
	var gitDecision, modelDecision access.Decision
	var gitLeaseID, modelLeaseID string
	modelExpiry := time.Now().Add(10 * time.Minute)
	connector := &bootstrap.Connector{Ledger: ledger, Client: &http.Client{Transport: transport, Timeout: 20 * time.Second},
		RouterURL: "https://atenet-router.ate-system.svc", Roots: actorRoots, Signer: signer,
		Token: func(context.Context) (string, error) { return token, nil }, Current: current,
		Authorize: func(ctx context.Context, tx pgx.Tx, runtime bootstrap.Runtime, challenge bootstrap.Challenge) error {
			if runtime.Image != image || runtime.WorkerPool != pool || runtime.SandboxClass != "SANDBOX_CLASS_GVISOR" || !dataSnapshots(runtime) {
				return bootstrap.ErrDenied
			}
			switch challenge.Phase {
			case bootstrap.PhaseSetup:
				if gitRepo == "" {
					return nil
				}
				checked, err := access.AuthorizeGitRead(ctx, tx, access.GitRead{
					OrganizationID: "synthetic-org", ProjectID: space, AttemptID: space + "/" + task,
					BindingID: gitBinding, GranteeKind: "user", GranteeID: "synthetic-operator",
					RepoURL: gitRepo, Commit: gitCommit, PolicyVersion: 1,
				})
				if err != nil || checked.DeliveryMode != "native_raw" {
					return access.ErrDenied
				}
				gitDecision = checked
				return nil
			case bootstrap.PhaseModel:
				checked, err := access.AuthorizeModelInvoke(ctx, tx, access.ModelInvoke{
					OrganizationID: "synthetic-org", ProjectID: space, AttemptID: space + "/" + task,
					BindingID: modelBinding, GranteeKind: "user", GranteeID: "synthetic-operator",
					Provider: modelProvider, Model: modelName, PolicyVersion: 1,
				})
				if err != nil || checked.DeliveryMode != "native_raw" {
					return access.ErrDenied
				}
				modelDecision = checked
				return nil
			default:
				return bootstrap.ErrDenied
			}
		},
	}
	if gitRepo != "" || modelBinding != "" {
		connector.Reserve = func(ctx context.Context, tx pgx.Tx, redeemed bootstrap.Redeemed) error {
			switch redeemed.Challenge.Phase {
			case bootstrap.PhaseSetup:
				if gitRepo == "" {
					return nil
				}
				id, err := access.ReserveGitLease(ctx, tx, access.LeaseRequest{
					OrganizationID: "synthetic-org", BindingID: gitBinding, ChallengeID: redeemed.ID,
					ClusterID: redeemed.Scope.ClusterID, AttemptID: redeemed.Scope.AttemptID,
					OwnerGeneration: redeemed.Scope.OwnerGeneration, ActorUID: redeemed.ActorUID,
					RepoURL: gitRepo, GuestExpiresAt: time.Unix(redeemed.Challenge.ExpiresAt, 0),
				})
				gitLeaseID = id
				return err
			case bootstrap.PhaseModel:
				if modelBinding == "" {
					return access.ErrDenied
				}
				id, err := access.ReserveModelLease(ctx, tx, access.ModelLeaseRequest{
					OrganizationID: "synthetic-org", BindingID: modelBinding, ChallengeID: redeemed.ID,
					ClusterID: redeemed.Scope.ClusterID, AttemptID: redeemed.Scope.AttemptID,
					OwnerGeneration: redeemed.Scope.OwnerGeneration, ActorUID: redeemed.ActorUID,
					Provider: modelProvider, Model: modelName, ExpiresAt: modelExpiry,
				})
				modelLeaseID = id
				return err
			default:
				return bootstrap.ErrDenied
			}
		}
		connector.Delivered = func(ctx context.Context, tx pgx.Tx, redeemed bootstrap.Redeemed) error {
			switch redeemed.Challenge.Phase {
			case bootstrap.PhaseSetup:
				if gitRepo == "" {
					return nil
				}
				return access.MarkLeaseDelivered(ctx, tx, "synthetic-org", gitLeaseID)
			case bootstrap.PhaseModel:
				return access.MarkLeaseDelivered(ctx, tx, "synthetic-org", modelLeaseID)
			default:
				return bootstrap.ErrDenied
			}
		}
	}
	if gitRepo != "" {
		connector.GitSetup = func(ctx context.Context, tx pgx.Tx, _ bootstrap.Runtime) (bootstrap.GitSetup, error) {
			if gitDecision.ConnectionID == "" || gitLeaseID == "" {
				return bootstrap.GitSetup{}, access.ErrDenied
			}
			secret, err := secretStore.ReadCurrent(ctx, tx, "synthetic-org", gitDecision.ConnectionID)
			if err != nil {
				return bootstrap.GitSetup{}, err
			}
			if err := access.MarkLeaseAttempt(ctx, tx, "synthetic-org", gitLeaseID, gitDecision.ConnectionID, secret.Version); err != nil {
				secret.Clear()
				return bootstrap.GitSetup{}, err
			}
			return bootstrap.GitSetup{RepoURL: gitRepo, Commit: gitCommit, Username: "blaxsmith-probe", Token: secret.Bytes}, nil
		}
	}
	if modelBinding != "" {
		connector.ModelCredential = func(ctx context.Context, tx pgx.Tx, _ bootstrap.Runtime) (bootstrap.ModelCredential, error) {
			if modelDecision.ConnectionID == "" || modelLeaseID == "" || !modelExpiry.After(time.Now()) {
				return bootstrap.ModelCredential{}, access.ErrDenied
			}
			secret, err := secretStore.ReadCurrent(ctx, tx, "synthetic-org", modelDecision.ConnectionID)
			if err != nil {
				return bootstrap.ModelCredential{}, err
			}
			if err := access.MarkLeaseAttempt(ctx, tx, "synthetic-org", modelLeaseID,
				modelDecision.ConnectionID, secret.Version); err != nil {
				secret.Clear()
				return bootstrap.ModelCredential{}, err
			}
			return bootstrap.ModelCredential{AttemptID: scope.AttemptID, Provider: modelProvider,
				ExpiresAt: modelExpiry, APIKey: secret.Bytes}, nil
		}
	}
	if phase == bootstrap.PhaseModel {
		err = connector.OpenModel(ctx, scope, expected)
	} else {
		err = connector.Open(ctx, scope, expected)
	}
	if err != nil {
		return err
	}
	connectionID, leaseID := gitDecision.ConnectionID, gitLeaseID
	if phase == bootstrap.PhaseModel {
		connectionID, leaseID = modelDecision.ConnectionID, modelLeaseID
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
		"phase": phase, "cluster_id": scope.ClusterID, "attempt_id": scope.AttemptID,
		"owner_generation": scope.OwnerGeneration, "actor_uid": expected.Actor.UID,
		"template_uid": expected.TemplateUID, "worker_pool": expected.WorkerPool,
		"snapshot_scope":    expected.SnapshotOnPause,
		"access_connection": connectionID, "access_lease": leaseID, "opened": true,
	}); err != nil {
		return err
	}
	completed = true
	return nil
}

func dataSnapshots(runtime bootstrap.Runtime) bool {
	return runtime.DataOnlySnapshots() && runtime.SnapshotStorage == "gs://ate-snapshots/blaxsmith/"
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
			SandboxConfig   struct{ SandboxClass string }
			SnapshotsConfig struct {
				OnPause, OnCommit, StorageLocation string
				OnResume                           struct{ FromData string }
			}
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
		WorkerPool:      a.Status.WorkerAssignment.WorkerPool,
		SnapshotOnPause: tmpl.SnapshotsConfig.OnPause, SnapshotOnCommit: tmpl.SnapshotsConfig.OnCommit,
		ResumeFromData: tmpl.SnapshotsConfig.OnResume.FromData, SnapshotStorage: tmpl.SnapshotsConfig.StorageLocation}, nil
}
