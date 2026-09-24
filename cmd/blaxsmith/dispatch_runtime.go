package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mjtechguy/blaxsmith/internal/access"
	"github.com/mjtechguy/blaxsmith/internal/axbridge"
	"github.com/mjtechguy/blaxsmith/internal/bootstrap"
	"github.com/mjtechguy/blaxsmith/internal/dispatch"
	"github.com/mjtechguy/blaxsmith/internal/identity"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

var dispatchSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)
var dispatchImage = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)
var dispatchResource = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

type dispatchConfig struct {
	axServer, axCLI, axCLISHA256                        string
	substrateCLI, substrateCLISHA256, substrateEndpoint string
	substrateTokenFile, substrateCAFile                 string
	routerURL, routerCAFile, actorCAFile                string
	bootstrapTokenFile, bootstrapSignerFile             string
	accessKeyFile                                       string
	clusterID, leaseTTL                                 string
	workerImage, workerPool, snapshotStorage            string
	workspace, gateway                                  string
	egressMode                                          string
}

type dispatchLookup func(string) (string, bool)

func parseDispatchConfig(enabled, allowOpenEgressDev bool, lookup dispatchLookup) (dispatchConfig, error) {
	if !enabled {
		if allowOpenEgressDev {
			return dispatchConfig{}, errors.New("--allow-open-egress-dev requires --enable-dispatch")
		}
		return dispatchConfig{}, nil
	}
	if lookup == nil {
		lookup = os.LookupEnv
	}
	var c dispatchConfig
	for _, item := range []struct {
		key string
		to  *string
	}{
		{"BLAXSMITH_DISPATCH_AX_SERVER", &c.axServer},
		{"BLAXSMITH_DISPATCH_AX_CLI", &c.axCLI},
		{"BLAXSMITH_DISPATCH_AX_CLI_SHA256", &c.axCLISHA256},
		{"BLAXSMITH_DISPATCH_SUBSTRATE_CLI", &c.substrateCLI},
		{"BLAXSMITH_DISPATCH_SUBSTRATE_CLI_SHA256", &c.substrateCLISHA256},
		{"BLAXSMITH_DISPATCH_SUBSTRATE_ENDPOINT", &c.substrateEndpoint},
		{"BLAXSMITH_DISPATCH_SUBSTRATE_TOKEN_FILE", &c.substrateTokenFile},
		{"BLAXSMITH_DISPATCH_SUBSTRATE_CA_FILE", &c.substrateCAFile},
		{"BLAXSMITH_DISPATCH_BOOTSTRAP_ROUTER_URL", &c.routerURL},
		{"BLAXSMITH_DISPATCH_BOOTSTRAP_ROUTER_CA_FILE", &c.routerCAFile},
		{"BLAXSMITH_DISPATCH_BOOTSTRAP_ACTOR_CA_FILE", &c.actorCAFile},
		{"BLAXSMITH_DISPATCH_BOOTSTRAP_TOKEN_FILE", &c.bootstrapTokenFile},
		{"BLAXSMITH_DISPATCH_BOOTSTRAP_SIGNER_FILE", &c.bootstrapSignerFile},
		{"BLAXSMITH_ACCESS_KEY_FILE", &c.accessKeyFile},
		{"BLAXSMITH_DISPATCH_CLUSTER_ID", &c.clusterID},
		{"BLAXSMITH_DISPATCH_LEASE_TTL", &c.leaseTTL},
		{"BLAXSMITH_DISPATCH_WORKER_IMAGE", &c.workerImage},
		{"BLAXSMITH_DISPATCH_WORKER_POOL", &c.workerPool},
		{"BLAXSMITH_DISPATCH_SNAPSHOT_STORAGE", &c.snapshotStorage},
		{"BLAXSMITH_DISPATCH_WORKSPACE", &c.workspace},
		{"BLAXSMITH_DISPATCH_GATEWAY", &c.gateway},
		{"BLAXSMITH_DISPATCH_EGRESS_MODE", &c.egressMode},
	} {
		value, ok := lookup(item.key)
		if !ok || strings.TrimSpace(value) == "" {
			return dispatchConfig{}, fmt.Errorf("--enable-dispatch requires %s", item.key)
		}
		*item.to = value
	}
	if c.egressMode != "exact" && c.egressMode != "open-dev" {
		return dispatchConfig{}, errors.New("dispatch egress mode must be explicitly set to exact or open-dev")
	}
	if c.egressMode == "open-dev" && !allowOpenEgressDev {
		return dispatchConfig{}, errors.New("open-dev egress requires the explicit --allow-open-egress-dev flag")
	}
	for _, path := range []string{c.axCLI, c.substrateCLI, c.substrateTokenFile, c.substrateCAFile,
		c.routerCAFile, c.actorCAFile, c.bootstrapTokenFile, c.bootstrapSignerFile,
		c.accessKeyFile} {
		if path == "" || !filepath.IsAbs(path) {
			return dispatchConfig{}, errors.New("dispatch requires absolute pinned-tool and secret file paths, including BLAXSMITH_ACCESS_KEY_FILE")
		}
	}
	axURL, err := url.Parse(c.axServer)
	if err != nil || axURL == nil {
		return dispatchConfig{}, errors.New("dispatch AX server URL is invalid")
	}
	axIP := net.ParseIP(axURLHostname(axURL))
	if axURL.Scheme != "http" || !validPort(axURL.Port()) || axURL.User != nil || axURL.Path != "" ||
		axURL.RawQuery != "" || axURL.Fragment != "" || axIP == nil || !axIP.IsLoopback() {
		return dispatchConfig{}, errors.New("dispatch AX server must be an HTTP URL on a literal loopback address with a port")
	}
	if host, port, err := net.SplitHostPort(c.substrateEndpoint); err != nil || host == "" || !validPort(port) {
		return dispatchConfig{}, errors.New("dispatch Substrate endpoint must be host:port")
	}
	router, err := url.Parse(c.routerURL)
	if err != nil || router == nil || router.Scheme != "https" || router.Hostname() == "" ||
		(router.Port() != "" && !validPort(router.Port())) || router.User != nil || router.Path != "" ||
		router.RawQuery != "" || router.Fragment != "" {
		return dispatchConfig{}, errors.New("dispatch bootstrap router must be an HTTPS origin")
	}
	if !dispatchSHA256.MatchString(c.axCLISHA256) || !dispatchSHA256.MatchString(c.substrateCLISHA256) ||
		!dispatchImage.MatchString(c.workerImage) || !dispatchResource.MatchString(c.workerPool) ||
		!dispatchResource.MatchString(c.workspace) || !dispatchResource.MatchString(c.gateway) ||
		c.clusterID == "" || len(c.clusterID) > 128 || c.snapshotStorage == "" || len(c.snapshotStorage) > 512 ||
		strings.ContainsAny(c.clusterID+c.snapshotStorage, " \t\r\n\x00") {
		return dispatchConfig{}, errors.New("dispatch requires pinned SHA-256 binaries and runner image, pool, storage, workspace, gateway, and cluster settings")
	}
	ttl, err := time.ParseDuration(c.leaseTTL)
	if err != nil || ttl <= 0 || ttl > time.Hour {
		return dispatchConfig{}, errors.New("dispatch model lease TTL must be positive and at most one hour")
	}
	return c, nil
}

func axURLHostname(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Hostname()
}

func validPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port > 0 && port <= 65535
}

type productDispatch struct {
	Dispatcher *dispatch.Dispatcher
	Activator  *dispatch.ModelActivator
	ax         axbridge.CLI
}

func newProductDispatch(ctx context.Context, config dispatchConfig, pool *pgxpool.Pool,
	store *workflow.Store, secrets *access.SecretStore) (*productDispatch, error) {
	if pool == nil || store == nil || secrets == nil {
		return nil, errors.New("dispatch requires workflow storage and access credential custody")
	}
	for _, item := range []struct{ path, digest string }{{config.axCLI, config.axCLISHA256}, {config.substrateCLI, config.substrateCLISHA256}} {
		if err := verifyPinnedExecutable(item.path, item.digest); err != nil {
			return nil, err
		}
	}
	for _, path := range []string{config.substrateTokenFile, config.bootstrapTokenFile} {
		data, err := readDispatchSecret(path, 16<<10)
		if err != nil {
			return nil, err
		}
		clear(data)
	}
	if err := validateDispatchTokenFile(config.substrateTokenFile); err != nil {
		return nil, err
	}
	data, err := readDispatchSecret(config.accessKeyFile, 32)
	if err != nil {
		return nil, err
	}
	clear(data)
	signer, err := identity.LoadSessionSigner(config.bootstrapSignerFile)
	if err != nil {
		return nil, errors.New("dispatch bootstrap signer is invalid")
	}
	actorRoots, err := loadDispatchRoots(config.actorCAFile)
	if err != nil {
		clear(signer)
		return nil, errors.New("dispatch actor attestation CA is invalid")
	}
	if _, err := loadDispatchRoots(config.substrateCAFile); err != nil {
		clear(signer)
		return nil, errors.New("dispatch Substrate CA is invalid")
	}
	routerRoots, err := loadDispatchRoots(config.routerCAFile)
	if err != nil {
		clear(signer)
		return nil, errors.New("dispatch router CA is invalid")
	}
	router, _ := url.Parse(config.routerURL)
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{
		Proxy: nil, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12,
			RootCAs: routerRoots, ServerName: router.Hostname()},
	}}
	ax := axbridge.CLI{AXPath: config.axCLI, Server: config.axServer, AtePath: config.substrateCLI,
		AteEndpoint: config.substrateEndpoint, AteTokenFile: config.substrateTokenFile, AteCAFile: config.substrateCAFile}
	activator := &dispatch.ModelActivator{DB: pool, Secrets: secrets, ClusterID: config.clusterID,
		LeaseTTL: mustParseDuration(config.leaseTTL), Actor: ax, Base: bootstrap.Connector{
			Client: client, RouterURL: config.routerURL,
			Token: func(context.Context) (string, error) { return dispatchToken(config.bootstrapTokenFile) },
			Roots: actorRoots, Signer: signer,
		}}
	bridge := &axbridge.Bridge{Workflow: store, AX: ax, Actor: ax,
		Image: config.workerImage, Pool: config.workerPool,
		Signer:  base64.StdEncoding.EncodeToString(signer.Public().(ed25519.PublicKey)),
		Storage: config.snapshotStorage, Workspace: config.workspace, Gateway: config.gateway,
		GatewayTemplate: config.gateway, GatewayEgressMode: config.egressMode,
		RevokeOwner: activator.RevokeOwner,
	}
	d := &dispatch.Dispatcher{Workflow: store, DB: pool, Secrets: secrets, Bridge: bridge,
		PreflightWorker: func(_ context.Context, approved workflow.ApprovedToolRuntime, _, _ string) error {
			if approved.Runtime.Image != config.workerImage || approved.WorkerPool != config.workerPool {
				return errors.New("approved worker image or pool differs from the pinned dispatch runtime")
			}
			return nil
		},
		PreflightActivation: activator.Preflight,
		Activate:            activator.Activate,
		ReleaseModel:        activator.ReleaseModel,
	}
	result := &productDispatch{Dispatcher: d, Activator: activator, ax: ax}
	if err := result.Preflight(ctx); err != nil {
		result.Close()
		return nil, err
	}
	return result, nil
}

func (d *productDispatch) Preflight(ctx context.Context) error {
	if d == nil || d.Dispatcher == nil || d.Activator == nil {
		return dispatch.ErrNotReady
	}
	if err := d.Activator.Preflight(ctx); err != nil {
		return dispatch.ErrNotReady
	}
	// Read-only probes verify both pinned clients and their configured control planes.
	const space, task = "blaxsmith-runtime-readiness", "dispatch-readiness"
	if _, err := d.ax.Get(ctx, space, task); !errors.Is(err, axbridge.ErrNotFound) {
		return errors.New("dispatch AX preflight failed")
	}
	if _, err := d.ax.Current(ctx, space, task); !errors.Is(err, axbridge.ErrNotFound) {
		return errors.New("dispatch Substrate preflight failed")
	}
	return nil
}

func (d *productDispatch) Close() {
	if d != nil && d.Activator != nil {
		clear(d.Activator.Base.Signer)
	}
}

func verifyPinnedExecutable(path, expected string) error {
	if !filepath.IsAbs(path) || !dispatchSHA256.MatchString(expected) {
		return errors.New("dispatch requires absolute pinned tool paths")
	}
	f, err := os.Open(path)
	if err != nil {
		return errors.New("pinned dispatch tool unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 {
		return errors.New("pinned dispatch tool must be a non-writable executable regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil || fmt.Sprintf("%x", h.Sum(nil)) != expected {
		return errors.New("dispatch tool SHA-256 does not match its configured pin")
	}
	return nil
}

func loadDispatchRoots(path string) (*x509.CertPool, error) {
	data, err := readDispatchFile(path, 1<<20, false)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return nil, errors.New("no CA certificates found")
	}
	return roots, nil
}

func readDispatchSecret(path string, max int64) ([]byte, error) {
	return readDispatchFile(path, max, true)
}

func readDispatchFile(path string, max int64, secret bool) ([]byte, error) {
	if !filepath.IsAbs(path) || max < 1 {
		return nil, errors.New("dispatch file path must be absolute")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("dispatch file unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > max ||
		(info.Mode().Perm()&0o027 != 0 && secret) || (info.Mode().Perm()&0o022 != 0 && !secret) {
		return nil, errors.New("dispatch file permissions or size are invalid")
	}
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil || int64(len(data)) > max {
		clear(data)
		return nil, errors.New("dispatch file read failed")
	}
	return data, nil
}

func dispatchToken(path string) (string, error) {
	data, err := readDispatchSecret(path, 16<<10)
	if err != nil {
		return "", err
	}
	defer clear(data)
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n\x00") {
		return "", errors.New("dispatch token file is invalid")
	}
	return token, nil
}

func validateDispatchTokenFile(path string) error {
	data, err := readDispatchSecret(path, 16<<10)
	if err != nil {
		return err
	}
	defer clear(data)
	token := bytes.TrimSpace(data)
	if len(token) == 0 || bytes.IndexAny(token, " \t\r\n\x00") >= 0 {
		return errors.New("dispatch token file is invalid")
	}
	return nil
}

func mustParseDuration(value string) time.Duration {
	d, _ := time.ParseDuration(value)
	return d
}
