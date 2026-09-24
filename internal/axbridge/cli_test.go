package axbridge

import (
	"context"
	"strings"
	"testing"
)

func TestAteCommandUsesExplicitDirectAuth(t *testing.T) {
	t.Setenv("KUBECTL_ATE_CA_FILE", "/stale/ca.pem")
	c := CLI{AtePath: "/usr/local/bin/kubectl-ate", AteEndpoint: "api.ate-system.svc:443",
		AteTokenFile: "/var/run/blaxsmith/token", AteCAFile: "/var/run/blaxsmith/ca.pem"}
	cmd, err := c.ateCommand(context.Background(), "get", "actor", "attempt", "-a", "pool", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	want := "--endpoint api.ate-system.svc:443 --token-file /var/run/blaxsmith/token get actor attempt -a pool -o json"
	if got := strings.Join(cmd.Args[1:], " "); got != want {
		t.Fatalf("arguments = %q, want %q", got, want)
	}
	ca := 0
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "KUBECTL_ATE_CA_FILE=") {
			ca++
			if entry != "KUBECTL_ATE_CA_FILE=/var/run/blaxsmith/ca.pem" {
				t.Fatalf("CA environment = %q", entry)
			}
		}
	}
	if ca != 1 {
		t.Fatalf("CA environment entries = %d, want 1", ca)
	}
}

func TestAteCommandRejectsPartialDirectAuth(t *testing.T) {
	_, err := (CLI{AtePath: "/usr/local/bin/kubectl-ate", AteEndpoint: "api.ate-system.svc:443"}).ateCommand(context.Background(), "get")
	if err == nil {
		t.Fatal("partial direct-auth configuration succeeded")
	}
}
