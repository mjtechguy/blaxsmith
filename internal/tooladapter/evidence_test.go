package tooladapter

import (
	"encoding/json"
	"github.com/mjtechguy/blaxsmith/internal/evidence"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvidenceFilesAndGateReceipt(t *testing.T) {
	useIXState(t)
	dir := t.TempDir()
	launchBytes, _ := json.Marshal(launch{Dir: dir, Run: []string{"test"}, Resume: []string{"test"}})
	if err := os.WriteFile(statePath("launch.json"), launchBytes, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# Report"), 0600); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"report","path":"report.md","kind":"report","title":"Review","renderer":"markdown"}`
	for range 2 {
		if code, _ := bx(t, "artifact", "--json", body); code != 0 {
			t.Fatalf("registration: %d", code)
		}
	}
	if code, out := bx(t, "artifacts"); code != 0 || !strings.Contains(out, evidence.SHA([]byte("# Report"))) {
		t.Fatalf("manifest: %d %s", code, out)
	}
	for _, path := range []string{"../outside", "/etc/passwd", ".git/config"} {
		if _, err := readEvidence(dir, path); err == nil {
			t.Fatalf("unsafe path read: %s", path)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "report.md"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvidence(dir, "link"); err == nil {
		t.Fatal("symlink read")
	}
	if err := os.WriteFile(filepath.Join(dir, "large"), make([]byte, evidence.MaxArtifact+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvidence(dir, "large"); err == nil {
		t.Fatal("oversized evidence read")
	}
	done := make(chan int, 1)
	go func() {
		code, _ := bx(t, "gate", "--json", `{"id":"gate1","check":"tests","verdict":"pass","summary":"ok","evidence":[]}`)
		done <- code
	}()
	waitAsked(t, "gate1")
	if code, _ := bx(t, "gate-receipt", "--json", `{"gate_id":"gate1","accepted":false,"reason":"missing"}`); code != 0 {
		t.Fatal(code)
	}
	if code := <-done; code != bxFailed {
		t.Fatalf("refused gate returned %d", code)
	}
	if code, _ := bx(t, "gate", "--json", `{"id":"gate1","check":"tests","verdict":"fail","summary":"changed","evidence":[]}`); code != bxInvalid {
		t.Fatalf("changed replay returned %d", code)
	}
}

func TestVerificationRejectsSymlinkRoot(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "checkout")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareVerification(link); err != ErrBlocked {
		t.Fatalf("symlink checkout: %v", err)
	}
}

// Run explicitly in a disposable Linux container with util-linux installed.
func TestVerificationLinuxIsolation(t *testing.T) {
	if os.Getenv("BLAXSMITH_TEST_VERIFICATION_ISOLATION") != "1" {
		t.Skip("requires disposable root Linux container")
	}
	source := t.TempDir()
	if err := os.Chmod(filepath.Dir(source), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"candidate", ".git/config"} {
		if err := os.WriteFile(filepath.Join(source, path), []byte("trusted"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := PrepareVerification(source); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/usr/bin/setpriv", "--reuid=60000", "--regid=60000", "--clear-groups", "--bounding-set=-all", "--inh-caps=-all", "--ambient-caps=-all", "--no-new-privs", "/bin/sh", "-c", `
set -eu
[ "$(cat "$1/candidate")" = trusted ]
! (echo changed > "$1/candidate")
! (echo changed > "$1/.git/config")
! (touch "$1/new-file")
! (chmod u+w "$1/candidate")
! (touch /tmp/blaxsmith-verification/1/foreign)
touch /tmp/blaxsmith-verification/0/own
`, "check", source)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolation failed: %v\n%s", err, out)
	}
	if err := PrepareVerification(source); err == nil {
		t.Fatal("reused verifier sandbox")
	}
}
