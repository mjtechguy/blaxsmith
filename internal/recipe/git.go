package recipe

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const maxArtifactBytes = 1 << 20
const maxBundleBytes = 16 << 20

type gitFile struct{ mode, object string }

type gitSource struct {
	repo, commit string
	files        map[string]gitFile
}

func openGit(ctx context.Context, repo, ref string) (gitSource, error) {
	g := gitSource{repo: repo, files: map[string]gitFile{}}
	if ref == "" || strings.HasPrefix(ref, "-") {
		return g, fmt.Errorf("a Git commit or ref is required")
	}
	out, err := g.run(ctx, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return g, err
	}
	g.commit = strings.TrimSpace(string(out))
	out, err = g.run(ctx, "ls-tree", "--full-tree", "-r", "-z", g.commit)
	if err != nil {
		return g, err
	}
	for record := range strings.SplitSeq(string(out), "\x00") {
		if record == "" {
			continue
		}
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return g, fmt.Errorf("invalid Git tree entry")
		}
		g.files[name] = gitFile{mode: fields[0], object: fields[2]}
	}
	return g, nil
}

func (g gitSource) read(ctx context.Context, name string) ([]byte, error) {
	if !validPath(name) {
		return nil, fmt.Errorf("invalid repository path %q", name)
	}
	f, ok := g.files[name]
	if !ok {
		return nil, fmt.Errorf("file %q is not committed at %s", name, g.commit)
	}
	if f.mode != "100644" && f.mode != "100755" {
		return nil, fmt.Errorf("file %q must be a regular Git blob; symlinks/submodules are unsupported", name)
	}
	out, err := g.run(ctx, "cat-file", "-s", f.object)
	if err != nil {
		return nil, err
	}
	size, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || size < 1 || size > maxArtifactBytes {
		return nil, fmt.Errorf("file %q must contain 1–%d bytes", name, maxArtifactBytes)
	}
	out, err = g.run(ctx, "cat-file", "blob", f.object)
	if err == nil && bytes.HasPrefix(out, []byte("version https://git-lfs.github.com/spec/v1")) {
		return nil, fmt.Errorf("file %q is an unresolved Git LFS pointer", name)
	}
	return out, err
}

func (g gitSource) run(ctx context.Context, args ...string) ([]byte, error) {
	base := []string{"--no-replace-objects", "--literal-pathspecs", "-C", g.repo}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	// The explicit repository is authoritative even when called from a Git hook.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	// No lazy network fetches, terminal prompts, replacement objects, or filters.
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	var stdout, stderr boundedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

type boundedBuffer struct{ buf bytes.Buffer }

func (b *boundedBuffer) String() string { return b.buf.String() }
func (b *boundedBuffer) Bytes() []byte  { return b.buf.Bytes() }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxBundleBytes-b.buf.Len() {
		return 0, fmt.Errorf("Git output exceeds %d bytes", maxBundleBytes)
	}
	return b.buf.Write(p)
}
