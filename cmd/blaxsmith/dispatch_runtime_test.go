package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDispatchConfigIsDisabledByDefaultAndRequiresExplicitOpenMode(t *testing.T) {
	if config, err := parseDispatchConfig(false, false, nil); err != nil || config.axServer != "" {
		t.Fatalf("dispatch should be off by default: %+v, %v", config, err)
	}
	if _, err := parseDispatchConfig(false, true, nil); err == nil {
		t.Fatal("open-egress development permission accepted while dispatch is disabled")
	}
	if _, err := parseDispatchConfig(true, false, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("incomplete opt-in dispatcher accepted")
	}

	dir := t.TempDir()
	vars := validDispatchVars(dir)
	lookup := func(key string) (string, bool) { value, ok := vars[key]; return value, ok }
	if config, err := parseDispatchConfig(true, false, lookup); err != nil || config.egressMode != "exact" {
		t.Fatalf("exact egress configuration rejected: %+v, %v", config, err)
	}
	vars["BLAXSMITH_DISPATCH_EGRESS_MODE"] = "open-dev"
	if _, err := parseDispatchConfig(true, false, lookup); err == nil {
		t.Fatal("open-dev egress accepted without the explicit CLI gate")
	}
	if config, err := parseDispatchConfig(true, true, lookup); err != nil || config.egressMode != "open-dev" {
		t.Fatalf("explicit open-dev egress configuration rejected: %+v, %v", config, err)
	}
	vars["BLAXSMITH_DISPATCH_AX_SERVER"] = "http://ax.example:18443"
	if _, err := parseDispatchConfig(true, false, lookup); err == nil {
		t.Fatal("non-loopback AX control endpoint accepted")
	}
	for key, value := range map[string]string{
		"BLAXSMITH_DISPATCH_AX_SERVER":            "http://127.0.0.1:not-a-port",
		"BLAXSMITH_DISPATCH_BOOTSTRAP_ROUTER_URL": "https://:443",
		"BLAXSMITH_DISPATCH_WORKER_IMAGE":         "registry.example/tool worker@sha256:" + strings.Repeat("c", 64),
	} {
		vars = validDispatchVars(dir)
		vars["BLAXSMITH_DISPATCH_EGRESS_MODE"] = "open-dev"
		vars[key] = value
		if _, err := parseDispatchConfig(true, true, lookup); err == nil {
			t.Errorf("malformed %s accepted", key)
		}
	}
}

func TestPinnedDispatchExecutableAndSecretFileChecks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ax")
	binary := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(path, binary, 0o500); err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(binary))
	if err := verifyPinnedExecutable(path, digest); err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedExecutable(path, strings.Repeat("0", 64)); err == nil {
		t.Fatal("changed executable digest accepted")
	}
	if err := os.Chmod(path, 0o770); err != nil {
		t.Fatal(err)
	}
	if err := verifyPinnedExecutable(path, digest); err == nil {
		t.Fatal("writable executable accepted")
	}

	secret := filepath.Join(dir, "token")
	if err := os.WriteFile(secret, []byte("synthetic-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := readDispatchSecret(secret, 128)
	if err != nil || string(data) != "synthetic-token" {
		t.Fatalf("private token rejected: %q, %v", data, err)
	}
	clear(data)
	if err := os.Chmod(secret, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readDispatchSecret(secret, 128); err == nil {
		t.Fatal("world-readable dispatch secret accepted")
	}
}

func validDispatchVars(dir string) map[string]string {
	vars := map[string]string{
		"BLAXSMITH_DISPATCH_AX_SERVER":                "http://127.0.0.1:18443",
		"BLAXSMITH_DISPATCH_AX_CLI":                   filepath.Join(dir, "ax"),
		"BLAXSMITH_DISPATCH_AX_CLI_SHA256":            strings.Repeat("a", 64),
		"BLAXSMITH_DISPATCH_SUBSTRATE_CLI":            filepath.Join(dir, "kubectl-ate"),
		"BLAXSMITH_DISPATCH_SUBSTRATE_CLI_SHA256":     strings.Repeat("b", 64),
		"BLAXSMITH_DISPATCH_SUBSTRATE_ENDPOINT":       "api.ate-system.svc:443",
		"BLAXSMITH_DISPATCH_SUBSTRATE_TOKEN_FILE":     filepath.Join(dir, "substrate-token"),
		"BLAXSMITH_DISPATCH_SUBSTRATE_CA_FILE":        filepath.Join(dir, "substrate-ca.pem"),
		"BLAXSMITH_DISPATCH_BOOTSTRAP_ROUTER_URL":     "https://router.ate-system.svc:443",
		"BLAXSMITH_DISPATCH_BOOTSTRAP_ROUTER_CA_FILE": filepath.Join(dir, "router-ca.pem"),
		"BLAXSMITH_DISPATCH_BOOTSTRAP_ACTOR_CA_FILE":  filepath.Join(dir, "actor-ca.pem"),
		"BLAXSMITH_DISPATCH_BOOTSTRAP_TOKEN_FILE":     filepath.Join(dir, "bootstrap-token"),
		"BLAXSMITH_DISPATCH_BOOTSTRAP_SIGNER_FILE":    filepath.Join(dir, "bootstrap-signer"),
		"BLAXSMITH_ACCESS_KEY_FILE":                   filepath.Join(dir, "access-key"),
		"BLAXSMITH_DISPATCH_CLUSTER_ID":               "development-cluster",
		"BLAXSMITH_DISPATCH_LEASE_TTL":                "15m",
		"BLAXSMITH_DISPATCH_WORKER_IMAGE":             "registry.example/tool-worker@sha256:" + strings.Repeat("c", 64),
		"BLAXSMITH_DISPATCH_WORKER_POOL":              "dev-workers",
		"BLAXSMITH_DISPATCH_SNAPSHOT_STORAGE":         "gs://blaxsmith-dev/snapshots/",
		"BLAXSMITH_DISPATCH_WORKSPACE":                "source",
		"BLAXSMITH_DISPATCH_GATEWAY":                  "public-egress",
		"BLAXSMITH_DISPATCH_EGRESS_MODE":              "exact",
	}
	return vars
}
