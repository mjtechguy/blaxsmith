package access

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
)

// APIKeyAuth marks a model API-key connection, the only kind that may carry a
// base URL. Subscriptions always talk to their provider directly.
const APIKeyAuth = "api_key"

// MaxBaseURL bounds a connection's base URL.
const MaxBaseURL = 512

// ErrBaseURL rejects a base URL that is not a plain https endpoint.
var ErrBaseURL = errors.New("base URL must be an https URL without credentials, query, or fragment")

// NormalizeBaseURL checks an API-key connection's optional base URL: an
// OpenAI- or Anthropic-compatible endpoint such as LiteLLM or a company
// gateway, used in place of the provider's own. It is the value the harness
// itself takes (Anthropic without /v1, OpenAI-compatible with /v1). Empty
// stays empty. The result is https, lower-case host, no userinfo, query, or
// fragment, and no trailing slash. It is not secret.
func NormalizeBaseURL(raw string) (string, error) { return normalizeBaseURL(raw, false) }

// normalizeBaseURL also accepts http to a loopback host when allowLoopback is
// set, for tests against a local server only.
func normalizeBaseURL(raw string, allowLoopback bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if len(raw) > MaxBaseURL || strings.ContainsAny(raw, " \t\r\n\x00\"'\\<>`{}|^") {
		return "", ErrBaseURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery ||
		u.Fragment != "" || u.RawFragment != "" || u.Hostname() == "" || u.EscapedPath() != u.Path ||
		strings.Contains(u.Path, "//") || strings.Contains(u.Path, "/../") || strings.HasSuffix(u.Path, "/..") {
		return "", ErrBaseURL
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
	case "http":
		if !allowLoopback || !loopbackHost(u.Hostname()) {
			return "", ErrBaseURL
		}
	default:
		return "", ErrBaseURL
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return "", ErrBaseURL
		}
	}
	return u.Scheme + "://" + host + strings.TrimRight(u.Path, "/"), nil
}

func loopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

// errEndpointAddress refuses an endpoint that resolves to a non-public address.
var errEndpointAddress = errors.New("base URL host does not resolve to a public address")

// endpointClient is the HTTP client for a connection's base URL. Anyone who
// may manage a connection chooses that URL, so the platform must not become
// a way into its own network: the dialer resolves the host itself, requires
// every address to be public IPv4 (gitfetch.PublicIPv4, the same test the Git
// fetch proxy uses), and connects to the address it checked, so a DNS answer
// that changes between check and connect (rebinding) cannot reach an internal
// address. Environment proxies are ignored and redirects are not followed.
func endpointClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
			if err != nil || len(addresses) == 0 {
				return nil, errEndpointAddress
			}
			for _, candidate := range addresses {
				if !gitfetch.PublicIPv4(candidate) {
					return nil, errEndpointAddress
				}
			}
			return dialer.DialContext(ctx, "tcp4", net.JoinHostPort(addresses[0].Unmap().String(), port))
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 20 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
