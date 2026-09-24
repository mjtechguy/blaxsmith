package runbranch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	t       *testing.T
	remote  Remote
	work    string
	base    string // input commit, on the remote's main
	ref     string
	scratch string
}

func (f *fixture) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.invalid",
		"-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	root := t.TempDir()
	f := &fixture{t: t, work: filepath.Join(root, "work"), ref: Ref("run-1"), scratch: root}
	remote := filepath.Join(root, "remote.git")
	f.git(root, "init", "-q", "--bare", remote)
	f.git(root, "init", "-q", f.work)
	f.git(f.work, "commit", "-q", "--allow-empty", "-m", "root")
	f.git(f.work, "commit", "-q", "--allow-empty", "-m", "base")
	f.base = f.git(f.work, "rev-parse", "HEAD")
	f.git(f.work, "push", "-q", remote, "HEAD:refs/heads/main")
	f.remote = Remote{URL: "file://" + remote}
	return f
}

// stage commits one change on from and bundles from..HEAD like the guest pane.
func (f *fixture) stage(from, name string, commits int) (string, string) {
	f.git(f.work, "checkout", "-q", "--detach", from)
	for i := range commits {
		if err := os.WriteFile(filepath.Join(f.work, name), []byte(name+string(rune('a'+i))), 0o600); err != nil {
			f.t.Fatal(err)
		}
		f.git(f.work, "add", "-A")
		f.git(f.work, "commit", "-q", "-m", name)
	}
	tip := f.git(f.work, "rev-parse", "HEAD")
	bundle := filepath.Join(f.scratch, name+".bundle")
	f.git(f.work, "bundle", "create", "-q", bundle, from+"..HEAD")
	return tip, bundle
}

func (f *fixture) remoteTip() string {
	out := f.git(f.scratch, "ls-remote", strings.TrimPrefix(f.remote.URL, "file://"), f.ref)
	tip, _, _ := strings.Cut(out, "\t")
	return tip
}

func TestDeliverPushesVerifiedStageCommit(t *testing.T) {
	f := newFixture(t)
	tip, bundle := f.stage(f.base, "one", 1)
	push := Push{Bundle: bundle, Ref: f.ref, Input: f.base, Revision: tip}
	if err := Deliver(t.Context(), f.remote, push); err != nil {
		t.Fatal(err)
	}
	if got := f.remoteTip(); got != tip {
		t.Fatalf("run branch at %q, want %q", got, tip)
	}
	// A crash after push retries with the old expectation: already there is success.
	if err := Deliver(t.Context(), f.remote, push); err != nil {
		t.Fatalf("idempotent redelivery: %v", err)
	}
	// A correction replaces the branch under a lease on the previous tip.
	next, nextBundle := f.stage(f.base, "two", 1)
	if err := Deliver(t.Context(), f.remote, Push{Bundle: nextBundle, Ref: f.ref, Input: f.base, Revision: next, Expected: tip}); err != nil {
		t.Fatal(err)
	}
	if got := f.remoteTip(); got != next {
		t.Fatalf("run branch at %q, want %q", got, next)
	}
}

func TestDeliverRejectsBadBundles(t *testing.T) {
	f := newFixture(t)
	tip, bundle := f.stage(f.base, "one", 1)
	cases := map[string]Push{
		"tip mismatch": {Bundle: bundle, Ref: f.ref, Input: f.base, Revision: f.base[:39] + "0"},
		"bad parent":   {Bundle: bundle, Ref: f.ref, Input: f.git(f.work, "rev-parse", f.base+"^"), Revision: tip},
	}
	two, twoBundle := f.stage(f.base, "two", 2)
	cases["two commits"] = Push{Bundle: twoBundle, Ref: f.ref, Input: f.base, Revision: two}
	for name, push := range cases {
		if err := Deliver(t.Context(), f.remote, push); !errors.Is(err, ErrRejected) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := f.remoteTip(); got != "" {
		t.Fatalf("rejected bundle reached the run branch: %s", got)
	}
}

func TestDeliverLeaseConflictFailsClosed(t *testing.T) {
	f := newFixture(t)
	other, otherBundle := f.stage(f.base, "other", 1)
	if err := Deliver(t.Context(), f.remote, Push{Bundle: otherBundle, Ref: f.ref, Input: f.base, Revision: other}); err != nil {
		t.Fatal(err)
	}
	// Someone else moved the branch; this platform still expects it absent.
	tip, bundle := f.stage(f.base, "mine", 1)
	if err := Deliver(t.Context(), f.remote, Push{Bundle: bundle, Ref: f.ref, Input: f.base, Revision: tip}); !errors.Is(err, ErrRejected) {
		t.Fatalf("lease conflict: %v", err)
	}
	if got := f.remoteTip(); got != other {
		t.Fatalf("lease conflict overwrote the branch: %s", got)
	}
}
