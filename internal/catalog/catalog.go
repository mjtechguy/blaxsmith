// Package catalog discovers exact CLI releases; it does not install or approve them.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxMetadataBytes = 24 << 20

var packages = map[string]string{
	"codex":       "@openai/codex",
	"claude-code": "@anthropic-ai/claude-code",
	"opencode":    "@opencode/cli",
}

type Release struct {
	Version     string    `json:"version"`
	PublishedAt time.Time `json:"published_at"`
	Integrity   string    `json:"integrity"`
	Tarball     string    `json:"tarball"`
}

type Result struct {
	Tool            string    `json:"tool"`
	Package         string    `json:"package"`
	Source          string    `json:"source"`
	FetchedAt       time.Time `json:"fetched_at"`
	LatestStable    string    `json:"latest_stable"`
	PublisherLatest string    `json:"publisher_latest"`
	PublisherStable string    `json:"publisher_stable,omitempty"`
	Releases        []Release `json:"releases"`
}

// Fetch reads a fixed, allowlisted npm package. The newest non-prerelease
// version is the default; publisher channel tags are shown separately.
func Fetch(ctx context.Context, client *http.Client, tool string, limit int) (Result, error) {
	name, ok := packages[tool]
	if !ok || client == nil || limit < 1 || limit > 100 {
		return Result{}, errors.New("unsupported tool or catalog limit")
	}
	source := "https://registry.npmjs.org/" + strings.ReplaceAll(name, "/", "%2f")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("fetch %s catalog: %w", tool, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("fetch %s catalog: HTTP %d", tool, resp.StatusCode)
	}
	if resp.Request == nil || resp.Request.URL.String() != source {
		return Result{}, fmt.Errorf("unexpected %s catalog redirect", tool)
	}
	var metadata struct {
		Name     string            `json:"name"`
		DistTags map[string]string `json:"dist-tags"`
		Time     map[string]string `json:"time"`
		Versions map[string]struct {
			Deprecated string `json:"deprecated"`
			Dist       struct {
				Integrity string `json:"integrity"`
				Tarball   string `json:"tarball"`
			} `json:"dist"`
		} `json:"versions"`
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes+1))
	if err := decoder.Decode(&metadata); err != nil {
		return Result{}, fmt.Errorf("decode %s catalog: %w", tool, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return Result{}, fmt.Errorf("unexpected trailing %s catalog data", tool)
	}
	if metadata.Name != name || len(metadata.Versions) == 0 {
		return Result{}, fmt.Errorf("unexpected %s package metadata", tool)
	}
	result := Result{Tool: tool, Package: name, Source: source, FetchedAt: time.Now().UTC(),
		PublisherLatest: metadata.DistTags["latest"], PublisherStable: metadata.DistTags["stable"]}
	for version, data := range metadata.Versions {
		if _, ok := stableParts(version); !ok || data.Deprecated != "" ||
			!strings.HasPrefix(data.Dist.Integrity, "sha512-") ||
			!strings.HasPrefix(data.Dist.Tarball, "https://registry.npmjs.org/"+name+"/-/") {
			continue
		}
		published, err := time.Parse(time.RFC3339Nano, metadata.Time[version])
		if err != nil {
			continue
		}
		result.Releases = append(result.Releases, Release{version, published, data.Dist.Integrity, data.Dist.Tarball})
	}
	slices.SortFunc(result.Releases, func(a, b Release) int {
		left, _ := stableParts(a.Version)
		right, _ := stableParts(b.Version)
		for i := range left {
			if left[i] > right[i] {
				return -1
			}
			if left[i] < right[i] {
				return 1
			}
		}
		return 0
	})
	if len(result.Releases) == 0 {
		return Result{}, fmt.Errorf("no eligible stable %s releases", tool)
	}
	result.LatestStable = result.Releases[0].Version
	if len(result.Releases) > limit {
		result.Releases = result.Releases[:limit]
	}
	return result, nil
}

func stableParts(version string) ([3]uint64, bool) {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return [3]uint64{}, false
	}
	var values [3]uint64
	for i, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return [3]uint64{}, false
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return [3]uint64{}, false
			}
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return [3]uint64{}, false
		}
		values[i] = value
	}
	return values, true
}
