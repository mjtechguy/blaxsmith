package tooladapter

import (
	"fmt"
	"net/url"
	"strings"
)

// Gateway points a brokered_gateway attempt's harness at the Blaxsmith model
// gateway (docs/model-gateway-plan.md §3 step 2). The leased credential is
// then a run-scoped gateway token, never a provider key. BaseURL is public
// installation configuration, so it may live in the task request.
type Gateway struct {
	BaseURL string `json:"base_url"`
}

func (g *Gateway) validate() error {
	if g == nil {
		return nil
	}
	u, err := url.Parse(g.BaseURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.Fragment != "" || len(g.BaseURL) > 256 ||
		strings.ContainsAny(g.BaseURL, " \r\n\x00") {
		return fmt.Errorf("%w: invalid model gateway base URL", ErrBlocked)
	}
	return nil
}

// familyURL is the gateway prefix for a provider: <base>/<provider>.
func (g *Gateway) familyURL(provider string) string { return g.BaseURL + "/" + provider }

// gatewayCredentialEnv replaces the one-key environment for a brokered attempt:
//   - Claude Code: ANTHROPIC_BASE_URL plus ANTHROPIC_AUTH_TOKEN (sent as
//     Authorization: Bearer). --bare reads only ANTHROPIC_API_KEY for auth, so
//     the token is also given there; both carry the same gateway token.
//   - Codex: OPENAI_BASE_URL for the built-in openai provider, OPENAI_API_KEY =
//     token.
//   - OpenCode: the provider's options.baseURL in the scoped opencode.json
//     (openCodeGatewayProvider); the key stays OPENCODE_API_KEY = token and is
//     read from the environment by the config, never written to disk.
func gatewayCredentialEnv(g *Gateway, harness, provider string, token []byte) []string {
	key := CredentialEnv(provider) + "=" + string(token)
	switch harness {
	case "claude-code":
		return []string{"ANTHROPIC_BASE_URL=" + g.familyURL(provider), "ANTHROPIC_AUTH_TOKEN=" + string(token), key}
	case "codex":
		return []string{"OPENAI_BASE_URL=" + g.familyURL(provider) + "/v1", key}
	default:
		return []string{key}
	}
}

// gatewayEnvKey reports the extra variables a brokered attempt may set.
func gatewayEnvKey(key string) bool {
	return key == "ANTHROPIC_BASE_URL" || key == "ANTHROPIC_AUTH_TOKEN" || key == "OPENAI_BASE_URL"
}

// openCodeGatewayProvider adds provider.<id>.options to OpenCode's scoped
// config: baseURL at the gateway, apiKey read from OPENCODE_API_KEY.
func openCodeGatewayProvider(config map[string]any, baseURL, model string) {
	if baseURL == "" {
		return
	}
	provider, _, _ := strings.Cut(model, "/")
	config["provider"] = map[string]any{provider: map[string]any{"options": map[string]any{
		"baseURL": baseURL + "/" + provider + "/v1", "apiKey": "{env:OPENCODE_API_KEY}"}}}
}
