package tooladapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// A leased API-key connection may name a base URL: an OpenAI- or
// Anthropic-compatible endpoint (LiteLLM, a company gateway) used in place of
// the provider's own. It is public configuration, frozen into the attempt's
// model binding at dispatch, so it rides in the task request, never in the
// credential file, and is never redacted. Subscriptions never have one. Each
// harness is pointed at it its own way:
//   - Claude Code: ANTHROPIC_BASE_URL=<base> (endpointEnv), key in
//     ANTHROPIC_API_KEY as usual.
//   - Codex: a named model provider in argv (codexEndpointArgs) whose env_key
//     is OPENAI_API_KEY. Codex 0.156.1's built-in openai provider ignores
//     OPENAI_BASE_URL, and a named provider with supports_websockets=false
//     uses HTTPS streaming only, which OpenAI-compatible proxies serve.
//   - OpenCode: provider.<id>.options.baseURL in its scoped opencode.json
//     (openCodeEndpointProvider), apiKey read from the provider's key env.

// maxModelBaseURL matches access.MaxBaseURL.
const maxModelBaseURL = 512

// validModelBaseURL re-checks the platform's normalization: https, a host, no
// userinfo, query, fragment, or trailing slash, bounded, and nothing that
// could break out of the TOML or JSON it is placed in.
func validModelBaseURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.HasSuffix(raw, "/") ||
		u.EscapedPath() != u.Path || len(raw) > maxModelBaseURL ||
		strings.ContainsAny(raw, " \t\r\n\x00\"'\\<>`{}|^") {
		return fmt.Errorf("%w: invalid model base URL", ErrBlocked)
	}
	return nil
}

// modelEnv is a leased key's tool environment: the key in its harness's
// variable and, with a base URL, what points the harness at it. Codex then
// reads the key from OPENAI_API_KEY, the endpoint provider's env_key.
func modelEnv(harness, provider, baseURL string, key []byte) ([]string, error) {
	name := nativeCredentialEnv(harness, provider)
	if baseURL == "" {
		return []string{name + "=" + string(key)}, nil
	}
	if validModelBaseURL(baseURL) != nil || bytes.HasPrefix(key, []byte(claudeSetupTokenPrefix)) {
		return nil, fmt.Errorf("%w: a base URL is only for an API key", ErrBlocked)
	}
	if harness == "codex" {
		name = "OPENAI_API_KEY"
	}
	return append([]string{name + "=" + string(key)}, endpointEnv(harness, baseURL)...), nil
}

// endpointEnv is the extra environment a base URL needs, beside the key.
func endpointEnv(harness, baseURL string) []string {
	if baseURL != "" && harness == "claude-code" {
		return []string{"ANTHROPIC_BASE_URL=" + baseURL}
	}
	return nil
}

// endpointEnvKey reports the public variables a base URL may set.
func endpointEnvKey(key string) bool { return key == "ANTHROPIC_BASE_URL" }

// codexEndpointProvider names the model provider a base URL selects.
const codexEndpointProvider = "blaxsmith-endpoint"

// codexEndpointArgs selects the base URL's model provider for exec and the
// takeover resume. The prompt stays the last exec argument.
func codexEndpointArgs(args, resume []string, baseURL string) ([]string, []string) {
	if baseURL == "" || len(args) == 0 {
		return args, resume
	}
	endpoint, _ := json.Marshal(baseURL)
	config := []string{"--config", `model_provider="` + codexEndpointProvider + `"`, "--config",
		`model_providers.` + codexEndpointProvider + `={name="Custom endpoint",base_url=` + string(endpoint) +
			`,env_key="OPENAI_API_KEY",wire_api="responses",supports_websockets=false}`}
	prompt := args[len(args)-1]
	args = append(append(append([]string(nil), args[:len(args)-1]...), config...), prompt)
	return args, append(append([]string(nil), resume...), config...)
}

// openCodeEndpointProvider adds provider.<id>.options to OpenCode's scoped
// config: baseURL at the endpoint, apiKey read from the provider's key env.
func openCodeEndpointProvider(config map[string]any, baseURL, model string) {
	provider, _, _ := strings.Cut(model, "/")
	if baseURL == "" || CredentialEnv(provider) == "" {
		return
	}
	config["provider"] = map[string]any{provider: map[string]any{"options": map[string]any{
		"baseURL": baseURL, "apiKey": "{env:" + CredentialEnv(provider) + "}"}}}
}
