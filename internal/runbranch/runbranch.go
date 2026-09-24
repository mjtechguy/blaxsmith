// Package runbranch verifies a stage's guest-written Git bundle and pushes it
// to the run branch in the project repository. The platform holds the write
// credential; agents never do (docs/interactive-sessions.md, "Code flow").
package runbranch

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MaxBundle bounds the guest bundle the platform reads (the guest caps it too).
const MaxBundle = 64 << 20

// ErrRejected means the bundle is not the single commit on the input commit
// that result.json names, or the run branch moved under us (lease conflict).
// Retrying the same bundle cannot succeed.
var ErrRejected = errors.New("run branch update rejected")

var commit = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Ref is the run branch for a run.
func Ref(runID string) string { return "refs/heads/blaxsmith/run-" + runID }

// Remote is the project repository and the platform's write credential. An
// empty Token sends no Authorization header (local test repositories).
type Remote struct {
	URL      string
	Username string
	Token    []byte
}

// Push is one verified delivery.
type Push struct {
	Bundle   string // local path of the guest bundle
	Ref      string // Ref(runID)
	Input    string // the attempt's frozen input commit
	Revision string // result.json revision
	Expected string // last tip this platform pushed, "" when the branch must not exist
}

// Deliver checks that the bundle holds exactly one commit whose parent is
// Input and whose tip is Revision, then pushes it with a force-with-lease on
// Expected. A branch already at Revision is success (a crash after push).
func Deliver(ctx context.Context, remote Remote, p Push) error {
	if !commit.MatchString(p.Input) || !commit.MatchString(p.Revision) || p.Revision == p.Input ||
		(p.Expected != "" && !commit.MatchString(p.Expected)) || !strings.HasPrefix(p.Ref, "refs/heads/blaxsmith/run-") {
		return ErrRejected
	}
	prerequisites, err := bundlePrerequisites(p.Bundle)
	if err != nil {
		return err
	}
	if len(prerequisites) != 1 || prerequisites[0] != p.Input {
		return fmt.Errorf("%w: bundle is not based on the input commit", ErrRejected)
	}
	dir, err := os.MkdirTemp("", "blaxsmith-runbranch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	git := gitIn(ctx, dir, remote)
	if _, err := git("init", "-q", "--bare"); err != nil {
		return err
	}
	heads, err := git("bundle", "list-heads", p.Bundle)
	if err != nil {
		return fmt.Errorf("%w: unreadable bundle", ErrRejected)
	}
	lines := strings.Split(strings.TrimSpace(heads), "\n")
	tip, head, _ := strings.Cut(lines[0], " ")
	if len(lines) != 1 || tip != p.Revision || head == "" {
		return fmt.Errorf("%w: bundle tip does not match result.json revision", ErrRejected)
	}
	current, err := git("ls-remote", "--", remote.URL, p.Ref)
	if err != nil {
		return fmt.Errorf("read run branch: %w", err)
	}
	current, _, _ = strings.Cut(strings.TrimSpace(current), "\t")
	if current == p.Revision {
		return nil
	}
	if current != p.Expected {
		return fmt.Errorf("%w: run branch is at %q, expected %q", ErrRejected, current, p.Expected)
	}
	// The input commit is the bundle's prerequisite; fetch only it.
	if _, err := git("fetch", "-q", "--depth=1", "--no-tags", "--", remote.URL, p.Input); err != nil {
		return fmt.Errorf("fetch input commit: %w", err)
	}
	if _, err := git("bundle", "verify", "-q", p.Bundle); err != nil {
		return fmt.Errorf("%w: bundle does not verify", ErrRejected)
	}
	if _, err := git("fetch", "-q", "--no-tags", p.Bundle, head+":refs/blaxsmith/tip"); err != nil {
		return fmt.Errorf("%w: bundle does not unpack", ErrRejected)
	}
	parents, err := git("rev-list", "--parents", "-n1", p.Revision)
	if err != nil || strings.Fields(parents)[0] != p.Revision || len(strings.Fields(parents)) != 2 ||
		strings.Fields(parents)[1] != p.Input {
		return fmt.Errorf("%w: bundle is not one commit on the input commit", ErrRejected)
	}
	if _, err := git("push", "-q", "--force-with-lease="+p.Ref+":"+p.Expected, "--", remote.URL,
		p.Revision+":"+p.Ref); err != nil {
		return fmt.Errorf("%w: push: %v", ErrRejected, err)
	}
	return nil
}

// bundlePrerequisites reads the v2/v3 bundle header's "-<oid>" lines.
func bundlePrerequisites(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 4096)
	first, err := r.ReadString('\n')
	if err != nil || (first != "# v2 git bundle\n" && first != "# v3 git bundle\n") {
		return nil, fmt.Errorf("%w: not a git bundle", ErrRejected)
	}
	var out []string
	for range 1024 {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("%w: truncated bundle header", ErrRejected)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			return out, nil
		case strings.HasPrefix(line, "-"):
			id, _, _ := strings.Cut(line[1:], " ")
			out = append(out, id)
		}
	}
	return nil, fmt.Errorf("%w: bundle header too long", ErrRejected)
}

// gitIn runs git in dir with no ambient config, prompts, or credential
// helpers. The token travels in the environment, never argv.
func gitIn(ctx context.Context, dir string, remote Remote) func(...string) (string, error) {
	config := [][2]string{{"credential.helper", ""}, {"protocol.allow", "user"}}
	if len(remote.Token) > 0 {
		basic := base64.StdEncoding.EncodeToString([]byte(remote.Username + ":" + string(remote.Token)))
		config = append(config, [2]string{"http.extraHeader", "Authorization: Basic " + basic})
	}
	env := []string{"HOME=" + dir, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + filepath.Join(dir, "no-global-config"), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false",
		"GIT_CONFIG_COUNT=" + strconv.Itoa(len(config))}
	for i, kv := range config {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed binary, separate argv
		cmd.Dir, cmd.Env, cmd.WaitDelay = dir, env, time.Second
		out, err := cmd.Output()
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(redact(string(exit.Stderr), remote.Token)))
			}
			return "", err
		}
		return string(out), nil
	}
}

func redact(s string, token []byte) string {
	if len(token) == 0 {
		return s
	}
	return strings.ReplaceAll(s, string(token), "[redacted]")
}
