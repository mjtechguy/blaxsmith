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
)

// Input selects committed content. Repo is local transport, never provenance.
// Scope is a directory containing the code an assignment may touch.
// PlatformFile is supplied by trusted application code, never a repository.
// Source identifies the immutable origin (for example goal:<id>@<revision>).
type PlatformFile struct {
	Data   []byte
	Source string
}

type Input struct {
	CheckpointID  string // Trusted platform provenance, never a repository claim.
	PlatformFiles map[string]PlatformFile
	Validators    map[string]Validator
	Repo          string
	Ref           string
	Recipe        string
	Scope         string
	// RecipeData, when set, is a library recipe version frozen under the
	// Recipe path label instead of reading that path from the commit. Its
	// prompts, skills, documents, and AGENTS.md still come from Git.
	RecipeData []byte
	// ResolveExtension returns the installed version for a stage template
	// reference. Nil refuses every template stage.
	ResolveExtension func(ctx context.Context, id, version string) (extension.Pin, error)
}

type Validator func(context.Context, map[string][]byte) (ValidationResult, error)

type ValidationResult struct {
	ID              string `json:"id"`
	Revision        string `json:"revision"`
	ValidatorSHA256 string `json:"validator_sha256"`
	Report          string `json:"report"`
}

type Source struct {
	CheckpointID string `json:"checkpoint_id,omitempty"`
	Commit       string `json:"commit"`
	Recipe       string `json:"recipe"`
	Scope        string `json:"scope"`
}

type Artifact struct {
	Source string `json:"source,omitempty"` // Empty = committed Git; otherwise a platform-owned input.
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"` // JSON base64 preserves the exact Git bytes.
}

type Bundle struct {
	Baseline      *RepositoryBaseline       `json:"repository_baseline,omitempty"`
	ProfileInputs map[string]ResolvedInputs `json:"profile_inputs,omitempty"`
	SchemaVersion string                    `json:"schema_version"`
	Source        Source                    `json:"source"`
	Recipe        Recipe                    `json:"recipe"`
	StageOrder    []string                  `json:"stage_order"`
	Artifacts     []Artifact                `json:"artifacts"`
	Validation    *ValidationResult         `json:"validation,omitempty"`
	// Extensions are the extension versions template stages use, sorted.
	Extensions []extension.Frozen `json:"extensions,omitempty"`
	Digest     string             `json:"digest,omitempty"`
}

// Freeze resolves a ref once, validates the selected recipe and optional integration inputs,
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
	for name, file := range in.PlatformFiles {
		if !validPath(name) || !strings.HasPrefix(name, ".blaxsmith/platform/") || name == in.Recipe || len(file.Data) == 0 || len(file.Data) > maxArtifactBytes || len(file.Source) < 1 || len(file.Source) > 256 || strings.ContainsAny(file.Source, "\r\n\x00") {
			return nil, fmt.Errorf("invalid platform input")
		}
		if _, exists := g.files[name]; exists {
			return nil, fmt.Errorf("platform input %q collides with Git", name)
		}
	}
	files := map[string][]byte{}
	total := 0
	add := func(name string) error {
		if _, ok := files[name]; ok {
			return nil
		}
		var data []byte
		if supplied, ok := in.PlatformFiles[name]; ok {
			data = slices.Clone(supplied.Data)
		} else {
			var err error
			data, err = g.read(ctx, name)
			if err != nil {
				return err
			}
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
	resolvedInputs, err := resolveInputs(ctx, g, &r, in.Scope, add, files)
	if err != nil {
		return nil, err
	}
	paths := slices.Clone(r.Documents)
	if r.Validation != nil {
		for _, file := range r.Validation.Inputs {
			paths = append(paths, file)
		}
	}
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
	for name := range in.PlatformFiles {
		if _, used := files[name]; !used {
			return nil, fmt.Errorf("unused platform input %q", name)
		}
	}
	var validation *ValidationResult
	if r.Validation != nil {
		validate := in.Validators[r.Validation.ID]
		if validate == nil {
			return nil, fmt.Errorf("validator %q is not installed", r.Validation.ID)
		}
		inputs := map[string][]byte{}
		for name, file := range r.Validation.Inputs {
			inputs[name] = slices.Clone(files[file])
		}
		result, err := validate(ctx, inputs)
		if err != nil {
			return nil, err
		}
		if result.ID != r.Validation.ID {
			return nil, fmt.Errorf("validator identity mismatch")
		}
		validation = &result
	}
	extensions, err := freezeExtensions(ctx, r, in.ResolveExtension)
	if err != nil {
		return nil, err
	}
	baseline := repositoryBaseline(ctx, g, in.Scope)
	b := &Bundle{Baseline: &baseline,
		SchemaVersion: "blaxsmith.bundle/v1alpha1", ProfileInputs: resolvedInputs,
		Source: Source{Commit: g.commit, Recipe: in.Recipe, Scope: in.Scope, CheckpointID: in.CheckpointID},
		Recipe: r, StageOrder: order, Validation: validation, Extensions: extensions,
	}
	for _, name := range sortedKeys(files) {
		b.Artifacts = append(b.Artifacts, Artifact{Path: name, SHA256: digest(files[name]), Data: files[name], Source: in.PlatformFiles[name].Source})
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
