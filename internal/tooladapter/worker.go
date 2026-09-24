package tooladapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/mjtechguy/blaxsmith/internal/gitfetch"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
)

const (
	WorkerBinary   = "/usr/local/bin/blaxsmith-tool-worker"
	CredentialFile = "/run/blaxsmith/agent-credential.json"
	maxTaskArg     = 120 << 10 // AX passes the JSON as one Linux argv element.
)

// Request is public task configuration, bound to the immutable AX task. It
// deliberately has no credential, connection ID, or lease token.
type Request struct {
	AttemptID       string           `json:"attempt_id"`
	RepositoryURL   string           `json:"repository_url"`
	SourceRef       string           `json:"source_ref"`
	SourceCommit    string           `json:"source_commit"`
	Runtime         Runtime          `json:"runtime"`
	Profile         recipe.Profile   `json:"profile"`
	Prompt          string           `json:"prompt"`
	FrozenArtifacts []ArtifactDigest `json:"frozen_artifacts,omitempty"`
	TimeoutSeconds  int              `json:"timeout_seconds"`
	MaxOutputBytes  int              `json:"max_output_bytes"`
}

// ArtifactDigest binds prompt context to regular files in the pinned checkout.
// Only public Git-authored text belongs here; no credentials or live config.
type ArtifactDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
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
	profile, err := frozenProfile(request)
	if err != nil {
		return nil, err
	}
	if _, err := Prepare(request.Runtime, profile, request.Prompt,
		time.Duration(request.TimeoutSeconds)*time.Second, request.MaxOutputBytes); err != nil {
		return nil, err
	}
	if _, err := credentialProvider(request.Profile); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > maxTaskArg {
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
	if len(encoded) == 0 || len(encoded) > maxTaskArg {
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
	if err := verifyFrozenArtifacts(workdir, request.FrozenArtifacts); err != nil {
		return nil, err
	}
	profile, err := frozenProfile(request)
	if err != nil {
		return nil, err
	}
	in, err := Prepare(request.Runtime, profile, request.Prompt,
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

func frozenProfile(request Request) (recipe.Profile, error) {
	seen := make(map[string]bool, len(request.FrozenArtifacts))
	for _, artifact := range request.FrozenArtifacts {
		if !filepath.IsLocal(artifact.Path) || path.Clean(artifact.Path) != artifact.Path ||
			strings.ContainsAny(artifact.Path, "\\\r\n\x00") || !sha256Hex.MatchString(artifact.SHA256) || seen[artifact.Path] ||
			len(seen) >= 256 {
			return recipe.Profile{}, fmt.Errorf("%w: invalid frozen artifact manifest", ErrBlocked)
		}
		seen[artifact.Path] = true
	}
	for _, name := range append(append([]string(nil), request.Profile.Instructions...), request.Profile.Skills...) {
		if !seen[name] {
			return recipe.Profile{}, fmt.Errorf("%w: instruction or skill is not frozen", ErrBlocked)
		}
	}
	profile := request.Profile
	profile.Instructions, profile.Skills = nil, nil
	return profile, nil
}

func verifyFrozenArtifacts(workdir string, artifacts []ArtifactDigest) error {
	allowedAgents := map[string]bool{}
	for _, artifact := range artifacts {
		name := filepath.Join(workdir, filepath.FromSlash(artifact.Path))
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 {
			return fmt.Errorf("%w: frozen artifact unavailable", ErrBlocked)
		}
		data, err := os.ReadFile(name)
		if err != nil || len(data) > 16<<20 {
			return fmt.Errorf("%w: frozen artifact unavailable", ErrBlocked)
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != artifact.SHA256 {
			return fmt.Errorf("%w: frozen artifact digest mismatch", ErrBlocked)
		}
		if path.Base(artifact.Path) == "AGENTS.md" {
			allowedAgents[artifact.Path] = true
		}
	}
	return filepath.WalkDir(workdir, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		base := entry.Name()
		if base == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if base == ".codex" || base == ".opencode" || base == ".claude" || base == ".agents" ||
			base == "opencode.json" || base == "opencode.jsonc" || base == "CLAUDE.md" || base == "CLAUDE.local.md" {
			return fmt.Errorf("%w: ambient project CLI configuration", ErrBlocked)
		}
		if base == "AGENTS.md" {
			relative, err := filepath.Rel(workdir, name)
			if err != nil || !allowedAgents[filepath.ToSlash(relative)] || !entry.Type().IsRegular() {
				return fmt.Errorf("%w: unlisted project instructions", ErrBlocked)
			}
		}
		return nil
	})
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
