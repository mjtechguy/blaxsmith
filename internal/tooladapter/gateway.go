package tooladapter

import (
	"encoding/json"
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
		strings.ContainsAny(g.BaseURL, " \r\n\x00\"'") {
		return fmt.Errorf("%w: invalid model gateway base URL", ErrBlocked)
	}
	return nil
}

// gatewayCredentialEnv replaces the one-key environment for a brokered
// attempt. Verified with real CLIs against a mock upstream through the
// gateway (internal/gateway/harness_probe_test.go):
//   - Claude Code 2.1.282 --bare honours ANTHROPIC_BASE_URL and sends
//     ANTHROPIC_AUTH_TOKEN as Authorization: Bearer. ANTHROPIC_API_KEY carries
//     the same token so apiKeySource matches the native path; it is never a
//     provider key.
//   - Codex 0.156.1 ignores OPENAI_BASE_URL; it gets a model provider in argv
//     (codexGatewayArgs) that reads the token from OPENAI_API_KEY.
//   - OpenCode reads options.baseURL from its scoped config
//     (openCodeGatewayProvider) and the token from OPENCODE_API_KEY.
func gatewayCredentialEnv(g *Gateway, harness, provider string, token []byte) []string {
	key := CredentialEnv(provider) + "=" + string(token)
	if harness == "claude-code" {
		return []string{"ANTHROPIC_BASE_URL=" + g.BaseURL + "/" + provider, "ANTHROPIC_AUTH_TOKEN=" + string(token), key}
	}
	return []string{key}
}

// gatewayEnvKey reports the extra variables a brokered attempt may set.
func gatewayEnvKey(key string) bool {
	return key == "ANTHROPIC_BASE_URL" || key == "ANTHROPIC_AUTH_TOKEN"
}

// codexGatewayArgs selects a gateway model provider for exec and the takeover
// resume. The built-in openai provider cannot be re-pointed without also
// opening Responses WebSockets, which the gateway does not proxy in G1; a
// named provider with supports_websockets=false uses HTTPS streaming only.
// The prompt stays the last exec argument.
func codexGatewayArgs(args, resume []string, baseURL string) ([]string, []string) {
	if baseURL == "" || len(args) == 0 {
		return args, resume
	}
	endpoint, _ := json.Marshal(baseURL + "/openai/v1")
	config := []string{"--config", `model_provider="blaxsmith-gateway"`, "--config",
		`model_providers.blaxsmith-gateway={name="Blaxsmith model gateway",base_url=` + string(endpoint) +
			`,env_key="OPENAI_API_KEY",wire_api="responses",supports_websockets=false}`}
	prompt := args[len(args)-1]
	args = append(append(append([]string(nil), args[:len(args)-1]...), config...), prompt)
	return args, append(append([]string(nil), resume...), config...)
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
