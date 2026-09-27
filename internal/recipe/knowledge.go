package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/repoinspect"
)

// RepositoryBaseline is always tied to the run's source. It contains no
// execution results and does not confer trust on suggested commands.
type RepositoryBaseline struct {
	Commit string `json:"commit"`
	repoinspect.Baseline
	ManifestsSHA256 map[string]string `json:"manifest_sha256,omitempty"`
}

func repositoryBaseline(ctx context.Context, g gitSource, scope string) RepositoryBaseline {
	contents := map[string][]byte{}
	hashes := map[string]string{}
	for _, name := range repoinspect.Wanted {
		if scope != "." {
			name = scope + "/" + name
		}
		if _, ok := g.files[name]; !ok {
			continue
		}
		if data, err := g.read(ctx, name); err == nil {
			contents[name] = data
			hashes[name] = digest(data)
		}
	}
	return RepositoryBaseline{Commit: g.commit, Baseline: repoinspect.InspectBaseline(g.regularFiles(), contents, scope), ManifestsSHA256: hashes}
}

type KnowledgeSnapshot struct {
	Schema        string            `json:"schema_version"`
	Title         string            `json:"title"`
	SourceCommit  string            `json:"source_commit"`
	Scope         string            `json:"scope"`
	Dependencies  map[string]string `json:"dependencies"`
	Claims        []KnowledgeClaim  `json:"claims"`
	Uncertainties []string          `json:"uncertainties"`
}
type KnowledgeClaim struct {
	ID        string         `json:"id"`
	Statement string         `json:"statement"`
	Kind      string         `json:"kind"` // observed or inferred; both are authored claims.
	Locations []CodeLocation `json:"locations"`
}
type CodeLocation struct {
	Path string `json:"path"`
	Line int    `json:"line"`
}
type ResolvedKnowledge struct {
	Scope            string           `json:"scope"`
	File             string           `json:"file"`
	SHA256           string           `json:"sha256"`
	Title            string           `json:"title"`
	SourceCommit     string           `json:"claimed_source_commit"`
	Status           string           `json:"status"` // current dependencies, stale, or unavailable.
	ChangedPaths     []string         `json:"changed_paths,omitempty"`
	UnavailablePaths []string         `json:"unavailable_paths,omitempty"`
	Claims           []KnowledgeClaim `json:"claims,omitempty"` // Only when dependencies match.
	Uncertainties    []string         `json:"uncertainties,omitempty"`
}

var knowledgeHash = regexp.MustCompile(`^[a-f0-9]{64}$`)
var knowledgeCommit = regexp.MustCompile(`^[a-f0-9]{40}$`)

type knowledgeDependency struct {
	SHA       string
	Lines     int
	Available bool
}

func resolveKnowledge(ctx context.Context, g gitSource, file, scope string, data []byte, dependencies map[string]knowledgeDependency) (ResolvedKnowledge, error) {
	out := ResolvedKnowledge{File: file, SHA256: digest(data), Status: "current dependencies"}
	var v KnowledgeSnapshot
	if len(data) > 128<<10 {
		return out, fmt.Errorf("knowledge snapshot %s exceeds 128 KiB", file)
	}
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return out, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil || d.Decode(new(any)) != io.EOF || v.Schema != "blaxsmith.knowledge/v1alpha1" || !knowledgeCommit.MatchString(v.SourceCommit) || (v.Scope != "." && !validPath(v.Scope)) || strings.TrimSpace(v.Title) == "" || len(v.Title) > 300 || len(v.Dependencies) < 1 || len(v.Dependencies) > 128 || len(v.Claims) > 64 || len(v.Uncertainties) > 32 {
		return out, fmt.Errorf("invalid knowledge snapshot %s", file)
	}
	if !relatedScope(v.Scope, scope) {
		return out, fmt.Errorf("knowledge %s scope %s does not intersect selected scope %s", file, v.Scope, scope)
	}
	for _, u := range v.Uncertainties {
		if strings.TrimSpace(u) == "" || len(u) > 4000 {
			return out, fmt.Errorf("invalid knowledge uncertainty in %s", file)
		}
	}
	out.Scope = v.Scope
	out.Title = v.Title
	out.SourceCommit = v.SourceCommit
	seen := map[string]bool{}
	for _, claim := range v.Claims {
		if !identifier.MatchString(claim.ID) || seen[claim.ID] || strings.TrimSpace(claim.Statement) == "" || len(claim.Statement) > 4000 || (claim.Kind != "observed" && claim.Kind != "inferred") || len(claim.Locations) < 1 || len(claim.Locations) > 16 {
			return out, fmt.Errorf("invalid knowledge claim in %s", file)
		}
		seen[claim.ID] = true
		for _, loc := range claim.Locations {
			if _, ok := v.Dependencies[loc.Path]; !ok || loc.Line < 1 || loc.Line > 1000000 {
				return out, fmt.Errorf("knowledge claim %s has an untracked location", claim.ID)
			}
		}
	}
	for _, name := range sortedKeys(v.Dependencies) {
		if !validPath(name) || !knowledgeHash.MatchString(v.Dependencies[name]) {
			return out, fmt.Errorf("invalid knowledge dependency in %s", file)
		}
		fact, known := dependencies[name]
		if !known {
			if len(dependencies) >= 256 {
				return out, fmt.Errorf("knowledge exceeds 256 distinct dependency files")
			}
			content, err := g.read(ctx, name)
			fact = knowledgeDependency{Available: err == nil}
			if err == nil {
				fact.SHA = digest(content)
				fact.Lines = bytes.Count(content, []byte("\n")) + 1
			}
			dependencies[name] = fact
		}
		if !fact.Available {
			out.UnavailablePaths = append(out.UnavailablePaths, name)
			continue
		}
		if fact.SHA != v.Dependencies[name] {
			out.ChangedPaths = append(out.ChangedPaths, name)
			continue
		}
		for _, claim := range v.Claims {
			for _, loc := range claim.Locations {
				if loc.Path == name && loc.Line > fact.Lines {
					return out, fmt.Errorf("knowledge location %s:%d exceeds file length", name, loc.Line)
				}
			}
		}
	}
	if len(out.UnavailablePaths) > 0 {
		out.Status = "unavailable"
	} else if len(out.ChangedPaths) > 0 {
		out.Status = "stale"
	}
	if out.Status == "current dependencies" {
		out.Claims = slices.Clone(v.Claims)
		out.Uncertainties = slices.Clone(v.Uncertainties)
	}
	return out, nil
}
