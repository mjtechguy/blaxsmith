package recipe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/guild"
)

// Input selects committed content. Repo is local transport, never provenance.
// Scope is a directory containing the code an assignment may touch.
type Input struct {
	Repo       string
	Ref        string
	Recipe     string
	Spec       string
	Transcript string
	Scope      string
	// RecipeData, when set, is a library recipe version frozen under the
	// Recipe path label instead of reading that path from the commit. Its
	// prompts, skills, spec, transcript, and AGENTS.md still come from Git.
	RecipeData []byte
	// ResolveExtension returns the installed version for a stage template
	// reference. Nil refuses every template stage.
	ResolveExtension func(ctx context.Context, id, version string) (extension.Pin, error)
}

type Source struct {
	Commit     string `json:"commit"`
	Recipe     string `json:"recipe"`
	Spec       string `json:"spec"`
	Transcript string `json:"transcript"`
	Scope      string `json:"scope"`
}

type Artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"` // JSON base64 preserves the exact Git bytes.
}

type Bundle struct {
	SchemaVersion string       `json:"schema_version"`
	Source        Source       `json:"source"`
	Recipe        Recipe       `json:"recipe"`
	StageOrder    []string     `json:"stage_order"`
	Artifacts     []Artifact   `json:"artifacts"`
	Guild         guild.Result `json:"guild"`
	// Extensions are the extension versions template stages use, sorted.
	Extensions []extension.Frozen `json:"extensions,omitempty"`
	Digest     string             `json:"digest,omitempty"`
}

// Freeze resolves a ref once, validates the selected recipe and Forge spec,
// and snapshots all declared instruction files plus applicable AGENTS.md files.
// It is a compilation result, not a signed or authorized execution request.
func Freeze(ctx context.Context, in Input) (*Bundle, error) {
	if in.Scope != "." && !validPath(in.Scope) {
		return nil, fmt.Errorf("scope must be a repository-relative directory")
	}
	g, err := openGit(ctx, in.Repo, in.Ref)
	if err != nil {
		return nil, err
	}
	foundScope := false
	for name, f := range g.files {
		if in.Scope == "." || strings.HasPrefix(name, in.Scope+"/") {
			foundScope = true
		}
		if f.mode == "160000" && relatedScope(name, in.Scope) {
			return nil, fmt.Errorf("scope includes unsupported submodule %q", name)
		}
	}
	if !foundScope {
		return nil, fmt.Errorf("scope %q contains no committed files", in.Scope)
	}
	files := map[string][]byte{}
	total := 0
	add := func(name string) error {
		if _, ok := files[name]; ok {
			return nil
		}
		data, err := g.read(ctx, name)
		if err != nil {
			return err
		}
		if !utf8.Valid(data) || strings.ContainsRune(string(data), 0) {
			return fmt.Errorf("artifact %q must be UTF-8 text without NUL bytes", name)
		}
		total += len(data)
		if total > maxBundleBytes || len(files) >= 256 {
			return fmt.Errorf("instruction bundle exceeds 16 MiB or 256 files")
		}
		files[name] = data
		return nil
	}
	if in.RecipeData != nil {
		if !validPath(in.Recipe) || !utf8.Valid(in.RecipeData) || strings.ContainsRune(string(in.RecipeData), 0) ||
			len(in.RecipeData) < 1 || len(in.RecipeData) > maxArtifactBytes {
			return nil, fmt.Errorf("library recipe needs a repository-relative path label and 1–%d bytes of UTF-8 text", maxArtifactBytes)
		}
		files[in.Recipe] = slices.Clone(in.RecipeData)
		total += len(in.RecipeData)
	} else if err := add(in.Recipe); err != nil {
		return nil, err
	}
	r, err := parse(files[in.Recipe])
	if err != nil {
		return nil, fmt.Errorf("recipe: %w", err)
	}
	order, err := r.validate()
	if err != nil {
		return nil, err
	}
	paths := []string{in.Spec, in.Transcript}
	for _, s := range r.Stages {
		if s.Prompt != "" {
			paths = append(paths, s.Prompt)
		}
	}
	for _, key := range sortedKeys(r.Profiles) {
		p := r.Profiles[key]
		paths = append(paths, p.Instructions...)
		paths = append(paths, p.Skills...)
	}
	for name := range g.files {
		if path.Base(name) == "AGENTS.md" && relatedScope(path.Dir(name), in.Scope) {
			paths = append(paths, name)
		}
	}
	slices.Sort(paths)
	if in.RecipeData != nil && slices.Contains(paths, in.Recipe) {
		return nil, fmt.Errorf("library recipe path label %q collides with a frozen input file", in.Recipe)
	}
	for _, name := range paths {
		if err := add(name); err != nil {
			return nil, err
		}
	}
	validation, err := guild.Validate(ctx, files[in.Spec], files[in.Transcript])
	if err != nil {
		return nil, err
	}
	extensions, err := freezeExtensions(ctx, r, in.ResolveExtension)
	if err != nil {
		return nil, err
	}
	b := &Bundle{
		SchemaVersion: "blaxsmith.bundle/v1alpha1",
		Source:        Source{g.commit, in.Recipe, in.Spec, in.Transcript, in.Scope},
		Recipe:        r, StageOrder: order, Guild: validation, Extensions: extensions,
	}
	for _, name := range sortedKeys(files) {
		b.Artifacts = append(b.Artifacts, Artifact{name, digest(files[name]), files[name]})
	}
	canonical, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	b.Digest = digest(canonical)
	return b, nil
}

// freezeExtensions resolves each template stage to one installed version and
// checks the template fits the stage: Phase 1 runs only embedded Claude Code
// templates.
func freezeExtensions(ctx context.Context, r Recipe, resolve func(context.Context, string, string) (extension.Pin, error)) ([]extension.Frozen, error) {
	frozen := map[string]extension.Frozen{}
	manifests := map[string]extension.Manifest{}
	for _, s := range r.Stages {
		if s.Template == "" {
			continue
		}
		id, version, name, _ := extension.ParseTemplateRef(s.Template)
		key := id + "@" + version
		if _, ok := frozen[key]; !ok {
			if resolve == nil {
				return nil, fmt.Errorf("stage %q uses extension %s, but no extensions are available here", s.ID, key)
			}
			pin, err := resolve(ctx, id, version)
			if err != nil {
				return nil, fmt.Errorf("stage %q: %w", s.ID, err)
			}
			f, m, err := pin.Freeze()
			if err != nil {
				return nil, fmt.Errorf("stage %q extension %s: %w", s.ID, key, err)
			}
			frozen[key], manifests[key] = f, m
		}
		t, ok := manifests[key].Template(name)
		switch {
		case !ok:
			return nil, fmt.Errorf("stage %q: extension %s has no stage template %q", s.ID, key, name)
		case t.Harness != r.Profiles[s.Profile].Harness:
			return nil, fmt.Errorf("stage %q: template %s requires harness %s; profile %q uses %s", s.ID, s.Template, t.Harness, s.Profile, r.Profiles[s.Profile].Harness)
		case !slices.Contains(t.Kinds, s.Kind):
			return nil, fmt.Errorf("stage %q: template %s does not fill %s stages", s.ID, s.Template, s.Kind)
		case t.Mode != "embedded" || t.Harness != "claude-code":
			return nil, fmt.Errorf("stage %q: only embedded Claude Code templates can run yet", s.ID)
		}
	}
	out := make([]extension.Frozen, 0, len(frozen))
	for _, key := range sortedKeys(frozen) {
		out = append(out, frozen[key])
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func relatedScope(a, b string) bool {
	return a == "." || b == "." || a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
