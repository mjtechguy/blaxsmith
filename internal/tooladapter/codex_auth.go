package tooladapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// CodexAuthPathFile names the one guest file that records where the
	// attempt's Codex auth.json lives, so the platform can replace it in place
	// when it renews the lease. The path is inside the attempt's temp home.
	CodexAuthPathFile = "codex-auth-path"
	maxCodexAuth      = 16 << 10
)

// credentialEnvNames are the provider keys (or, for a brokered_gateway
// attempt, the gateway token) a lease can put in the tool environment. The
// redactors hide their values; codexAuthSecrets adds the tokens of a
// delivered Codex sign-in, which never enter the environment. The gateway's
// ANTHROPIC_BASE_URL is public configuration (gatewayEnvKey).
var credentialEnvNames = []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENCODE_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// ParseCodexAuth checks a delivered Codex ChatGPT sign-in: bounded JSON with
// an access token and a present, empty refresh token (the platform keeps the
// real one, so the CLI can never spend or rotate the owner's login). It
// returns the token values to redact.
func ParseCodexAuth(data []byte) ([][]byte, error) {
	if len(data) == 0 || len(data) > maxCodexAuth || bytes.IndexByte(data, 0) >= 0 {
		return nil, fmt.Errorf("%w: invalid Codex sign-in", ErrBlocked)
	}
	var file struct {
		Tokens *struct {
			IDToken      string  `json:"id_token"`
			AccessToken  string  `json:"access_token"`
			RefreshToken *string `json:"refresh_token"`
		} `json:"tokens"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&file); err != nil || d.Decode(new(any)) != io.EOF || file.Tokens == nil ||
		file.Tokens.RefreshToken == nil || *file.Tokens.RefreshToken != "" || file.Tokens.AccessToken == "" ||
		strings.ContainsAny(file.Tokens.AccessToken+file.Tokens.IDToken, "\r\n\x00") {
		return nil, fmt.Errorf("%w: invalid Codex sign-in", ErrBlocked)
	}
	secrets := [][]byte{[]byte(file.Tokens.AccessToken)}
	if file.Tokens.IDToken != "" {
		secrets = append(secrets, []byte(file.Tokens.IDToken))
	}
	return secrets, nil
}

// writeCodexAuth places the sign-in at <home>/.codex/auth.json (0600, in a
// 0700 directory) and records that path for in-place renewal.
func writeCodexAuth(home string, data []byte) (string, error) {
	codexHome := filepath.Join(home, ".codex")
	if err := os.Mkdir(codexHome, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(codexHome, "auth.json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(StateDir(), 0700); err != nil {
		return "", err
	}
	if err := writeAtomic(statePath(CodexAuthPathFile), []byte(path+"\n"), 0600); err != nil {
		return "", err
	}
	return codexHome, nil
}

var codexSecretCache struct {
	sync.Mutex
	path    string
	size    int64
	mod     time.Time
	secrets [][]byte
}

// codexAuthSecrets returns the tokens in $CODEX_HOME/auth.json, re-read only
// when the platform has replaced the file.
func codexAuthSecrets() [][]byte {
	home := os.Getenv("CODEX_HOME")
	if home == "" {
		return nil
	}
	path := filepath.Join(home, "auth.json")
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	c := &codexSecretCache
	c.Lock()
	defer c.Unlock()
	if c.path == path && c.size == info.Size() && c.mod.Equal(info.ModTime()) {
		return c.secrets
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c.secrets // keep redacting the last known tokens
	}
	defer clear(data)
	if secrets, err := ParseCodexAuth(data); err == nil {
		c.path, c.size, c.mod = path, info.Size(), info.ModTime()
		c.secrets = append(secrets, c.secrets...) // replaced tokens stay redacted too
		if len(c.secrets) > 16 {
			c.secrets = c.secrets[:16]
		}
	}
	return c.secrets
}

// leasedSecrets are every leased credential value visible to this process.
func leasedSecrets() [][]byte {
	var out [][]byte
	for _, name := range credentialEnvNames {
		if key := os.Getenv(name); len(key) >= 8 {
			out = append(out, []byte(key))
		}
	}
	for _, secret := range codexAuthSecrets() {
		if len(secret) >= 8 {
			out = append(out, secret)
		}
	}
	return out
}

func redactSecrets(p []byte, secrets [][]byte) []byte {
	for _, secret := range secrets {
		if len(secret) > 0 {
			p = bytes.ReplaceAll(p, secret, []byte("[redacted]"))
		}
	}
	return p
}
