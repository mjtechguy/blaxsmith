// Package gitfetch materializes one public, pinned Git input without giving
// Git ambient credentials or a route to private network addresses.
package gitfetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Source struct {
	Directory string
	Commit    string
	Close     func() error
}

var (
	gitSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	gitRef     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	gitCommit  = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)

// IsCommit reports whether commit is a canonical lowercase SHA-1 or SHA-256 ID.
func IsCommit(commit string) bool { return gitCommit.MatchString(commit) }

// Validate rejects non-public transport choices before a workflow attempt is
// reserved. Fetch additionally pins each connection through its local proxy.
func Validate(rawURL, ref string) error {
	if len(rawURL) == 0 || len(rawURL) > 2048 || len(ref) > 128 {
		return errors.New("unsupported public Git source")
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.EscapedPath() != u.Path ||
		(u.Hostname() != "github.com" && u.Hostname() != "gitlab.com") || u.Host != u.Hostname() {
		return errors.New("unsupported public Git source")
	}
	segments := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if !strings.HasPrefix(u.Path, "/") || len(segments) < 2 || (u.Hostname() == "github.com" && len(segments) != 2) {
		return errors.New("unsupported public Git source")
	}
	for _, segment := range segments {
		if len(segment) > 128 || !gitSegment.MatchString(segment) || strings.Contains(segment, "..") ||
			strings.HasSuffix(segment, ".") || segment == ".git" {
			return errors.New("unsupported public Git source")
		}
	}
	if ref != "" && (!gitRef.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "/.")) {
		return errors.New("unsupported public Git ref")
	}
	return nil
}

// Fetch accepts only public GitHub/GitLab HTTPS sources. A loopback CONNECT
// proxy resolves and dials the checked public IP itself, so a DNS change
// between validation and Git's connection cannot redirect into the cluster.
func Fetch(ctx context.Context, rawURL, ref string) (Source, error) {
	if err := Validate(rawURL, ref); err != nil {
		return Source{}, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return Source{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "blaxsmith-git-")
	if err != nil {
		return Source{}, err
	}
	clean := func() error { return os.RemoveAll(dir) }
	proxy, err := newProxy(ctx, u.Hostname())
	if err != nil {
		_ = clean()
		return Source{}, err
	}
	defer proxy.Close()
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1"}
	args := []string{"-c", "http.proxy=http://" + proxy.Addr().String(), "-c", "http.followRedirects=false",
		"-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "init.templateDir=/dev/null"}
	run := func(extra ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", append(args, extra...)...) // #nosec G204 -- fixed executable, separate argv, restricted proxy/env
		cmd.Env = env
		cmd.Dir = dir
		cmd.WaitDelay = time.Second
		out, err := cmd.CombinedOutput()
		if len(out) > 8192 {
			out = out[:8192]
		}
		if err != nil {
			return nil, fmt.Errorf("git %s: %w: %s", extra[0], err, strings.TrimSpace(string(out)))
		}
		return out, nil
	}
	if _, err := run("init", "--quiet", dir); err != nil {
		_ = clean()
		return Source{}, err
	}
	if _, err := run("fetch", "--quiet", "--depth=1", "--no-tags", rawURL, ref); err != nil {
		_ = clean()
		return Source{}, err
	}
	commit, err := run("rev-parse", "--verify", "FETCH_HEAD^{commit}")
	if err != nil {
		_ = clean()
		return Source{}, err
	}
	return Source{Directory: filepath.Clean(dir), Commit: strings.TrimSpace(string(commit)), Close: clean}, nil
}

// Checkout materializes exactly the commit admitted by the server into an
// empty workspace. A moved ref is an error; no CLI runs against another tree.
func Checkout(ctx context.Context, rawURL, ref, commit, workdir string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if !IsCommit(commit) || !filepath.IsAbs(workdir) || workdir == "/" {
		return errors.New("invalid pinned Git workspace")
	}
	info, err := os.Lstat(workdir)
	if err != nil || !info.IsDir() {
		return errors.New("Git workspace must be an empty directory")
	}
	entries, err := os.ReadDir(workdir)
	if err != nil || len(entries) != 0 {
		return errors.New("Git workspace must be an empty directory")
	}
	source, err := Fetch(ctx, rawURL, ref)
	if err != nil {
		return err
	}
	defer source.Close()
	if source.Commit != commit {
		return errors.New("Git ref moved from frozen commit")
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + source.Directory, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_NO_LAZY_FETCH=1"}
	run := func(dir string, args ...string) error {
		command := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- fixed Git executable, separate validated argv, local source only
		command.Env, command.Dir, command.WaitDelay = env, dir, time.Second
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("materialize pinned Git workspace: %w: %s", err, strings.TrimSpace(string(output)))
		}
		return nil
	}
	if err := run(source.Directory, "-c", "core.hooksPath=/dev/null", "update-ref", "refs/heads/blaxsmith-pinned", commit); err != nil {
		return err
	}
	if err := run(source.Directory, "symbolic-ref", "HEAD", "refs/heads/blaxsmith-pinned"); err != nil {
		return err
	}
	if err := run(source.Directory, "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null",
		"-c", "init.templateDir=/dev/null", "clone", "--no-local", "--no-checkout", "--quiet", source.Directory, workdir); err != nil {
		return err
	}
	if err := run(workdir, "-c", "core.hooksPath=/dev/null", "checkout", "--detach", "--force", "--quiet", commit); err != nil {
		return err
	}
	check := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "HEAD^{commit}") // #nosec G204 -- fixed Git command in pinned local checkout
	check.Env, check.Dir, check.WaitDelay = env, workdir, time.Second
	got, err := check.Output()
	if err != nil || strings.TrimSpace(string(got)) != commit {
		return errors.New("Git workspace commit does not match frozen input")
	}
	return nil
}

type proxyServer struct {
	listener net.Listener
	server   *http.Server
}

func (p *proxyServer) Addr() net.Addr { return p.listener.Addr() }
func (p *proxyServer) Close() error   { return p.server.Close() }

func newProxy(ctx context.Context, host string) (*proxyServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &proxyServer{listener: listener}
	p.server = &http.Server{ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != net.JoinHostPort(host, "443") {
			http.Error(w, "destination denied", http.StatusForbidden)
			return
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(r.Context(), host)
		if err != nil {
			http.Error(w, "destination unavailable", http.StatusBadGateway)
			return
		}
		var upstream net.Conn
		for _, candidate := range addresses {
			address, ok := netip.AddrFromSlice(candidate.IP)
			if !ok || !PublicIPv4(address) {
				continue
			}
			upstream, err = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(r.Context(), "tcp", net.JoinHostPort(address.String(), "443"))
			if err == nil {
				break
			}
		}
		if upstream == nil {
			http.Error(w, "public destination unavailable", http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = upstream.Close()
			http.Error(w, "proxy unsupported", http.StatusInternalServerError)
			return
		}
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			_ = upstream.Close()
			return
		}
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			_ = client.Close()
			_ = upstream.Close()
			return
		}
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close() }()
		go func() { _, _ = io.Copy(client, upstream); _ = client.Close() }()
	})}
	go func() { _ = p.server.Serve(listener) }()
	go func() { <-ctx.Done(); _ = p.Close() }()
	return p, nil
}

// PublicIPv4 is the restricted address test shared by source fetch and AX
// gateway preflight. IPv6 transition routes are deliberately unsupported.
func PublicIPv4(address netip.Addr) bool {
	address = address.Unmap()
	// This first release uses IPv4 only. IPv6 transition/NAT64 ranges can
	// tunnel a public-looking address into a private IPv4 destination.
	if !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24",
		"192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4",
		"240.0.0.0/4"} {
		if netip.MustParsePrefix(block).Contains(address) {
			return false
		}
	}
	return true
}
