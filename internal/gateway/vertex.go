package gateway

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Anthropic on Google Vertex AI (§2): the same Messages body, with
// anthropic_version in the body and the model in the URL, sent with an
// OAuth access token the gateway gets by signing a service-account JWT
// (RFC 7523). Vertex streams Anthropic SSE unchanged.

const (
	googleTokenURL = "https://oauth2.googleapis.com/token"
	vertexScope    = "https://www.googleapis.com/auth/cloud-platform"
)

// ServiceAccount is the part of a Google service-account key the gateway
// uses. Stored as a connection secret (auth_method gcp_service_account).
type ServiceAccount struct {
	Type         string `json:"type"`
	ClientEmail  string `json:"client_email"`
	PrivateKeyID string `json:"private_key_id"`
	PrivateKey   string `json:"private_key"`
	key          *rsa.PrivateKey
}

// ParseServiceAccount validates a service-account key JSON.
func ParseServiceAccount(raw []byte) (ServiceAccount, error) {
	var sa ServiceAccount
	if err := json.Unmarshal(raw, &sa); err != nil || sa.Type != "service_account" || sa.ClientEmail == "" ||
		len(sa.ClientEmail) > 256 || strings.ContainsAny(sa.ClientEmail+sa.PrivateKeyID, " \r\n\x00\"") {
		return ServiceAccount{}, errors.New("expected a Google service-account key (type service_account)")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return ServiceAccount{}, errors.New("service-account private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok {
		return ServiceAccount{}, errors.New("service-account private_key must be an RSA key")
	}
	sa.key = key
	return sa, nil
}

// assertion is the signed JWT exchanged for an access token.
func (sa ServiceAccount) assertion(audience string, now time.Time) (string, error) {
	header, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": sa.PrivateKeyID})
	claims, _ := json.Marshal(map[string]any{"iss": sa.ClientEmail, "scope": vertexScope, "aud": audience,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, sa.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

// googleTokens caches access tokens by the hash of the service-account key,
// so rotating the connection's secret uses a new token at once.
type googleTokens struct {
	mu     sync.Mutex
	tokens map[[32]byte]googleToken
}

type googleToken struct {
	value   string
	expires time.Time
}

// token returns a cached access token with at least a minute left, or
// exchanges a fresh JWT at tokenURL.
func (g *googleTokens) token(ctx context.Context, client *http.Client, tokenURL string, raw []byte) (string, error) {
	id := sha256.Sum256(raw)
	g.mu.Lock()
	if g.tokens == nil {
		g.tokens = map[[32]byte]googleToken{}
	}
	cached, ok := g.tokens[id]
	g.mu.Unlock()
	now := time.Now()
	if ok && cached.expires.Sub(now) > time.Minute {
		return cached.value, nil
	}
	sa, err := ParseServiceAccount(raw)
	if err != nil {
		return "", err
	}
	jwt, err := sa.assertion(tokenURL, now)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {jwt}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("google token exchange: %w", err)
	}
	defer response.Body.Close()
	var body struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if response.StatusCode != http.StatusOK ||
		json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&body) != nil || body.AccessToken == "" {
		return "", fmt.Errorf("google token exchange: HTTP %d", response.StatusCode)
	}
	token := googleToken{value: body.AccessToken, expires: now.Add(time.Duration(max(body.ExpiresIn, 60)) * time.Second)}
	g.mu.Lock()
	if len(g.tokens) > 1000 {
		g.tokens = map[[32]byte]googleToken{}
	}
	g.tokens[id] = token
	g.mu.Unlock()
	return token.value, nil
}

func vertexEndpoint(region string) string {
	if region == "global" {
		return "https://aiplatform.googleapis.com"
	}
	return "https://" + region + "-aiplatform.googleapis.com"
}

// vertexBody drops the model (it is in the URL) and sets Vertex's
// anthropic_version; stream stays in the body.
func vertexBody(body []byte) ([]byte, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, false, err
	}
	var stream bool
	_ = json.Unmarshal(fields["stream"], &stream)
	delete(fields, "model")
	fields["anthropic_version"] = json.RawMessage(`"vertex-2023-10-16"`)
	out, err := json.Marshal(fields)
	return out, stream, err
}

func vertexPath(project, region, model string, stream bool) string {
	method := ":rawPredict"
	if stream {
		method = ":streamRawPredict"
	}
	return "/v1/projects/" + url.PathEscape(project) + "/locations/" + url.PathEscape(region) +
		"/publishers/anthropic/models/" + url.PathEscape(model) + method
}
