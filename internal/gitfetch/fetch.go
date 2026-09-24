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
	"strings"
	"time"
)

type Source struct {
	Directory string
	Commit    string
	Close     func() error
}

// Fetch accepts only public GitHub/GitLab HTTPS sources. A loopback CONNECT
// proxy resolves and dials the checked public IP itself, so a DNS change
// between validation and Git's connection cannot redirect into the cluster.
func Fetch(ctx context.Context, rawURL, ref string) (Source, error) {
	if ref == "" {
		ref = "HEAD"
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.Fragment != "" ||
		(u.Hostname() != "github.com" && u.Hostname() != "gitlab.com") || !strings.HasPrefix(u.Path, "/") ||
		strings.Contains(u.Path, "..") || ref == "" || strings.HasPrefix(ref, "-") {
		return Source{}, errors.New("unsupported public Git source")
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
			if !ok || !publicIP(address) {
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

func publicIP(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "192.0.0.0/24",
		"192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4",
		"240.0.0.0/4", "2001:db8::/32"} {
		if netip.MustParsePrefix(block).Contains(address) {
			return false
		}
	}
	return true
}
