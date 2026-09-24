package tooladapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

const (
	WorkerBinary   = "/usr/local/bin/blaxsmith-tool-worker"
	CredentialFile = "/run/blaxsmith/agent-credential.json"
)

// Request is public task configuration, bound to the immutable AX task. It
// deliberately has no credential, connection ID, or lease token.
type Request struct {
	AttemptID      string         `json:"attempt_id"`
	RepositoryURL  string         `json:"repository_url"`
	SourceRef      string         `json:"source_ref"`
	SourceCommit   string         `json:"source_commit"`
	Runtime        Runtime        `json:"runtime"`
	Profile        recipe.Profile `json:"profile"`
	Prompt         string         `json:"prompt"`
	TimeoutSeconds int            `json:"timeout_seconds"`
	MaxOutputBytes int            `json:"max_output_bytes"`
}

// Command prepares a task argv without a shell or a secret. The AX bridge
// must also pin Runtime.Image to the observed actor image.
func Command(request Request) ([]string, error) {
	if request.AttemptID == "" || len(request.AttemptID) > 128 || strings.ContainsAny(request.AttemptID, " \t\r\n\x00") {
		return nil, ErrBlocked
	}
	if err := gitfetch.Validate(request.RepositoryURL, request.SourceRef); err != nil || !gitCommit.MatchString(request.SourceCommit) {
		return nil, fmt.Errorf("%w: invalid frozen public Git source", ErrBlocked)
	}
	if _, err := Prepare(request.Runtime, request.Profile, request.Prompt,
		time.Duration(request.TimeoutSeconds)*time.Second, request.MaxOutputBytes); err != nil {
		return nil, err
	}
	if _, err := credentialProvider(request.Profile); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > 1<<20 {
		return nil, ErrBlocked
	}
	return []string{WorkerBinary, string(body)}, nil
}

type credential struct {
	AttemptID string `json:"attempt_id"`
	Provider  string `json:"provider"`
	ExpiresAt int64  `json:"expires_at"`
	APIKey    string `json:"api_key"`
}

// Execute is the AX task command's pod-side entrypoint. A missing, stale, or
// mismatched bootstrap credential prevents the CLI from starting.
func Execute(ctx context.Context, encoded, workdir, credentialPath string) ([]byte, error) {
	return execute(ctx, encoded, workdir, credentialPath, gitfetch.Checkout)
}

func execute(ctx context.Context, encoded, workdir, credentialPath string,
	checkout func(context.Context, string, string, string, string) error) ([]byte, error) {
	if len(encoded) == 0 || len(encoded) > 1<<20 {
		return nil, ErrBlocked
	}
	var request Request
	if err := strictJSON([]byte(encoded), &request); err != nil {
		return nil, ErrBlocked
	}
	if _, err := Command(request); err != nil {
		return nil, err
	}
	provider, err := credentialProvider(request.Profile)
	if err != nil {
		return nil, err
	}
	key, expiry, err := readCredential(credentialPath, request.AttemptID, provider)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if checkout == nil || checkout(ctx, request.RepositoryURL, request.SourceRef, request.SourceCommit, workdir) != nil {
		return nil, fmt.Errorf("%w: pinned source checkout failed", ErrBlocked)
	}
	in, err := Prepare(request.Runtime, request.Profile, request.Prompt,
		time.Duration(request.TimeoutSeconds)*time.Second, request.MaxOutputBytes)
	if err != nil {
		return nil, err
	}
	variable := "OPENAI_API_KEY="
	if provider == "anthropic" {
		variable = "ANTHROPIC_API_KEY="
	}
	leaseContext, cancel := context.WithDeadline(ctx, expiry)
	defer cancel()
	output, err := Run(leaseContext, in, workdir, []string{variable + string(key)})
	return bytes.ReplaceAll(output, key, []byte("[redacted]")), err
}

func credentialProvider(profile recipe.Profile) (string, error) {
	switch profile.Harness {
	case "codex":
		return "openai", nil
	case "claude-code":
		return "anthropic", nil
	case "opencode":
		provider, _, _ := strings.Cut(profile.Model, "/")
		if provider == "openai" || provider == "anthropic" {
			return provider, nil
		}
	}
	return "", fmt.Errorf("%w: provider credential adapter unavailable", ErrBlocked)
}

func readCredential(path, attemptID, provider string) ([]byte, time.Time, error) {
	if path == "" {
		return nil, time.Time{}, ErrBlocked
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: credential unavailable", ErrBlocked)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 16384 {
		return nil, time.Time{}, fmt.Errorf("%w: credential file is not private", ErrBlocked)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%w: credential unavailable", ErrBlocked)
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(body) > 16384 {
		return nil, time.Time{}, ErrBlocked
	}
	var value credential
	if err := strictJSON(body, &value); err != nil || value.AttemptID != attemptID || value.Provider != provider ||
		value.ExpiresAt <= time.Now().Unix() || value.ExpiresAt > time.Now().Add(time.Hour).Unix() ||
		len(value.APIKey) == 0 || len(value.APIKey) > 8192 || strings.ContainsAny(value.APIKey, "\r\n\x00") {
		return nil, time.Time{}, fmt.Errorf("%w: credential is stale or mismatched", ErrBlocked)
	}
	return []byte(value.APIKey), time.Unix(value.ExpiresAt, 0), nil
}

func strictJSON(body []byte, into any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(into); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrBlocked
	}
	return nil
}
