package gateway

import (
	"net/http"
	"slices"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/access"
)

// hopByHop headers are connection-scoped (RFC 9110 §7.6.1) and never forwarded.
var hopByHop = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Connection": true, "Te": true, "Trailer": true,
	"Transfer-Encoding": true, "Upgrade": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
}

// strippedRequest are client headers that carry credentials, pick an
// upstream account, or describe our own network (§12). Everything else
// (anthropic-version, anthropic-beta, prompt-caching and tool options,
// OpenAI-Beta, User-Agent, session ids) is forwarded unchanged: the request
// is sent as itself.
var strippedRequest = map[string]bool{
	"Authorization": true, "X-Api-Key": true, "Api-Key": true, "Cookie": true,
	"Openai-Organization": true, "Openai-Project": true, "Anthropic-Organization-Id": true, "Chatgpt-Account-Id": true,
	"Forwarded": true, "X-Real-Ip": true, "Via": true, "Host": true, "Content-Length": true,
	// The transport negotiates compression itself so usage can be metered;
	// the client receives an identity-encoded body.
	"Accept-Encoding": true,
}

// strippedRequestPrefixes cover cloud auth (SigV4, Google, Azure) and proxy
// metadata headers.
var strippedRequestPrefixes = []string{"X-Amz-", "X-Goog-", "X-Ms-", "X-Forwarded-", "X-Blaxsmith-"}

func copyRequestHeaders(dst, src http.Header) {
	for name, values := range src {
		name = http.CanonicalHeaderKey(name)
		if hopByHop[name] || strippedRequest[name] || hasPrefix(name, strippedRequestPrefixes) {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
}

// claudeOAuthBeta is the beta a Claude subscription token requires when sent
// as an OAuth bearer (docs/model-gateway-plan.md §6.1).
const claudeOAuthBeta = "oauth-2025-04-20"

// injectCredential adds the route's real credential; the only one sent.
// A member's own Claude setup-token goes as Authorization: Bearer with the
// OAuth beta instead of x-api-key; the owner's Codex sign-in goes as a
// bearer with its ChatGPT account id, as Codex itself sends it.
func injectCredential(h http.Header, family, path string, key []byte, authMethod, accountID string) {
	switch {
	case authMethod == access.ClaudeSetupTokenAuth:
		h.Set("Authorization", "Bearer "+string(key))
		betas := headerList(h, "Anthropic-Beta")
		if !slices.Contains(betas, claudeOAuthBeta) {
			betas = append(betas, claudeOAuthBeta)
		}
		h.Set("Anthropic-Beta", strings.Join(betas, ","))
	case authMethod == access.CodexSubscriptionAuth:
		h.Set("Authorization", "Bearer "+string(key))
		if accountID != "" {
			h.Set("Chatgpt-Account-Id", accountID)
		}
	case family == "anthropic":
		h.Set("X-Api-Key", string(key))
	case family == "opencode" || family == "opencode-go":
		h.Set("Authorization", "Bearer "+string(key))
		if path == "/v1/messages" { // Zen's Anthropic-format endpoint also reads x-api-key.
			h.Set("X-Api-Key", string(key))
		}
	default:
		h.Set("Authorization", "Bearer "+string(key))
	}
}

// strippedResponse would reveal the upstream account (organization ids,
// account emails) or set state for our upstream session.
var strippedResponse = map[string]bool{
	"Anthropic-Organization-Id": true, "Openai-Organization": true, "Openai-Project": true,
	"Set-Cookie": true, "Content-Length": true, "Content-Encoding": true, "Alt-Svc": true,
}

func copyResponseHeaders(dst, src http.Header) {
	for name, values := range src {
		name = http.CanonicalHeaderKey(name)
		lower := strings.ToLower(name)
		if hopByHop[name] || strippedResponse[name] ||
			strings.Contains(lower, "organization") || strings.Contains(lower, "account") || strings.Contains(lower, "email") {
			continue
		}
		dst[name] = append([]string(nil), values...)
	}
	dst.Set("Cache-Control", "no-store")
	dst.Set("X-Accel-Buffering", "no") // ask any reverse proxy in front of us not to buffer SSE.
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}
