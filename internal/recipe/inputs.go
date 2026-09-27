package recipe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mjtechguy/blaxsmith/internal/repoinspect"
)

// Inputs is committed guidance, never execution authority. Commands are
// suggestions; only the separately frozen verification policy executes checks.
type Inputs struct {
	ProjectMode string                     `json:"project_mode,omitempty"`
	Knowledge   []string                   `json:"knowledge,omitempty"`
	Schema      string                     `json:"schema_version"`
	Rules       []ScopedRule               `json:"rules,omitempty"`
	Stack       *StackProfile              `json:"stack,omitempty"`
	Agents      map[string]AgentDefinition `json:"agents,omitempty"`
}
type ScopedRule struct {
	ID       string            `json:"id"`
	Scope    string            `json:"scope"`
	File     string            `json:"file"`
	Settings map[string]string `json:"settings,omitempty"`
}
type StackProfile struct {
	Name           string                `json:"name"`
	PackageManager string                `json:"package_manager,omitempty"`
	Prerequisites  []string              `json:"prerequisites,omitempty"`
	Conventions    []string              `json:"conventions,omitempty"`
	Examples       []string              `json:"examples,omitempty"`
	Commands       []repoinspect.Command `json:"commands,omitempty"`
}
type AgentDefinition struct {
	Responsibility string   `json:"responsibility"`
	Instructions   []string `json:"instructions,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	Harnesses      []string `json:"harnesses,omitempty"`
	Models         []string `json:"models,omitempty"`
	Capabilities   []string `json:"capabilities,omitempty"`
	Inputs         []string `json:"input_expectations,omitempty"`
	Outputs        []string `json:"output_expectations,omitempty"`
	Completion     []string `json:"completion_criteria"`
}
type ResolvedInputs struct {
	ProjectMode string              `json:"selected_project_mode,omitempty"`
	Knowledge   []ResolvedKnowledge `json:"knowledge,omitempty"`
	File        string              `json:"file"`
	SHA256      string              `json:"sha256"`
	Commit      string              `json:"commit"`
	Scope       string              `json:"scope"`
	Rules       []ScopedRule        `json:"rules,omitempty"`
	Stack       *StackProfile       `json:"stack,omitempty"`
	AgentID     string              `json:"agent_id,omitempty"`
	Agent       *AgentDefinition    `json:"agent,omitempty"`
	Diagnostics []string            `json:"diagnostics,omitempty"`
}

func parseInputs(data []byte) (Inputs, error) {
	var v Inputs
	if len(data) > 64<<10 || !utf8.Valid(data) || bytes.ContainsRune(data, 0) {
		return v, fmt.Errorf("inputs require at most 64 KiB of UTF-8 text without NUL")
	}
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return v, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return v, err
	}
	if d.Decode(new(any)) != io.EOF || v.Schema != "blaxsmith.inputs/v1alpha1" || len(v.Rules) > 64 || len(v.Agents) > 32 {
		return v, fmt.Errorf("inputs require blaxsmith.inputs/v1alpha1, at most 64 rules and 32 agents")
	}
	text := func(s string) bool {
		return strings.TrimSpace(s) != "" && len(s) <= 4000 && !strings.ContainsRune(s, 0)
	}
	list := func(v []string) bool {
		return len(v) <= 64 && !slices.ContainsFunc(v, func(s string) bool { return !text(s) })
	}
	paths := func(v []string) bool {
		return len(v) <= 64 && !slices.ContainsFunc(v, func(s string) bool { return !validPath(s) })
	}
	if v.ProjectMode != "" && !slices.Contains([]string{"greenfield", "brownfield", "mixed"}, v.ProjectMode) {
		return v, fmt.Errorf("unsupported project_mode")
	}
	if !paths(v.Knowledge) || len(v.Knowledge) > 8 {
		return v, fmt.Errorf("knowledge requires at most 8 snapshot paths")
	}
	seen := map[string]bool{}
	for _, r := range v.Rules {
		if !identifier.MatchString(r.ID) || seen[r.ID] || (r.Scope != "." && !validPath(r.Scope)) || !validPath(r.File) || len(r.Settings) > 32 {
			return v, fmt.Errorf("invalid or duplicate scoped rule %q", r.ID)
		}
		seen[r.ID] = true
		for k, val := range r.Settings {
			if !identifier.MatchString(k) || !text(val) {
				return v, fmt.Errorf("invalid setting in rule %q", r.ID)
			}
		}
	}
	if s := v.Stack; s != nil {
		if !text(s.Name) || (s.PackageManager != "" && !text(s.PackageManager)) || !list(s.Prerequisites) || !list(s.Conventions) || !paths(s.Examples) {
			return v, fmt.Errorf("invalid stack profile")
		}
		// Reuse the project command contract; decoding cannot run these argv lists.
		b, _ := json.Marshal(repoinspect.ProjectFile{Version: 1, Verification: s.Commands})
		if _, err := repoinspect.ParseProjectFile(b); err != nil {
			return v, fmt.Errorf("stack commands: %w", err)
		}
	}
	for id, a := range v.Agents {
		if !identifier.MatchString(id) || !text(a.Responsibility) || !paths(a.Instructions) || !paths(a.Skills) || !list(a.Harnesses) || !list(a.Models) || !list(a.Capabilities) || !list(a.Inputs) || !list(a.Outputs) || len(a.Completion) == 0 || !list(a.Completion) {
			return v, fmt.Errorf("invalid agent definition %q", id)
		}
		for _, h := range a.Harnesses {
			if h != "codex" && h != "claude-code" && h != "opencode" {
				return v, fmt.Errorf("agent %q requests unsupported harness %q", id, h)
			}
		}
		for _, c := range a.Capabilities {
			if !slices.Contains([]string{"read_repository", "ask_user", "publish_artifacts", "write_candidate"}, c) {
				return v, fmt.Errorf("agent %q requests unsupported capability %q", id, c)
			}
		}
	}
	return v, nil
}

// resolveInputs is shared by every factory. It reads only the pinned Git tree.
// It preserves explicit profile choices and rejects contradictions before admission.
func resolveInputs(ctx context.Context, g gitSource, r *Recipe, scope string, add func(string) error, files map[string][]byte) (map[string]ResolvedInputs, error) {
	// Configuration and every referenced guidance file must come from Git,
	// even when the factory also supplies trusted platform documents.
	committed := func(name string) error {
		f, ok := g.files[name]
		if !ok || (f.mode != "100644" && f.mode != "100755") {
			return fmt.Errorf("input %q must be a committed regular file", name)
		}
		return add(name)
	}
	resolved := map[string]ResolvedInputs{}
	knowledgeCache := map[string]ResolvedKnowledge{}
	dependencies := map[string]knowledgeDependency{}
	for _, name := range sortedKeys(r.Profiles) {
		p := r.Profiles[name]
		if p.Inputs == "" {
			continue
		}
		if err := committed(p.Inputs); err != nil {
			return nil, err
		}
		v, err := parseInputs(files[p.Inputs])
		if err != nil {
			return nil, fmt.Errorf("profile %s inputs %s: %w", name, p.Inputs, err)
		}
		out := ResolvedInputs{ProjectMode: v.ProjectMode, File: p.Inputs, SHA256: digest(files[p.Inputs]), Commit: g.commit, Scope: scope, Stack: v.Stack, AgentID: p.Agent}
		for _, file := range v.Knowledge {
			if err := committed(file); err != nil {
				return nil, err
			}
			knowledge, cached := knowledgeCache[file]
			if !cached {
				var err error
				knowledge, err = resolveKnowledge(ctx, g, file, scope, files[file], dependencies)
				if err != nil {
					return nil, err
				}
				knowledgeCache[file] = knowledge
			}
			out.Knowledge = append(out.Knowledge, knowledge)
		}
		for _, rule := range v.Rules {
			if relatedScope(rule.Scope, scope) {
				out.Rules = append(out.Rules, rule)
			}
		}
		for i, a := range out.Rules {
			for _, b := range out.Rules[:i] {
				if !relatedScope(a.Scope, b.Scope) {
					continue
				}
				for _, key := range sortedKeys(a.Settings) {
					if value, ok := b.Settings[key]; ok && value != a.Settings[key] {
						return nil, fmt.Errorf("conflicting setting %q: %s rule %s (%s) and rule %s (%s); resolve explicitly", key, p.Inputs, a.ID, a.File, b.ID, b.File)
					}
				}
			}
			if err := committed(a.File); err != nil {
				return nil, err
			}
		}
		if len(out.Rules) > 1 {
			out.Diagnostics = append(out.Diagnostics, "Prose conflicts require review; structured settings are checked for overlapping scopes.")
		}
		if p.Agent != "" {
			a, ok := v.Agents[p.Agent]
			if !ok {
				return nil, fmt.Errorf("profile %s: agent %q is not in %s", name, p.Agent, p.Inputs)
			}
			if len(a.Harnesses) > 0 && !slices.Contains(a.Harnesses, p.Harness) {
				return nil, fmt.Errorf("agent %s does not support selected harness %s", p.Agent, p.Harness)
			}
			if len(a.Models) > 0 && !slices.Contains(a.Models, p.Model) {
				return nil, fmt.Errorf("agent %s does not support selected model %s", p.Agent, p.Model)
			}
			for _, s := range r.Stages {
				if s.Profile == name && slices.Contains(a.Capabilities, "write_candidate") && s.Kind != "implement" {
					return nil, fmt.Errorf("agent %s requests write_candidate but stage %s is %s", p.Agent, s.ID, s.Kind)
				}
			}
			for _, file := range append(slices.Clone(a.Instructions), a.Skills...) {
				if err := committed(file); err != nil {
					return nil, err
				}
			}
			p.Instructions = append(slices.Clone(a.Instructions), p.Instructions...)
			p.Skills = append(slices.Clone(a.Skills), p.Skills...)
			p.Instructions = uniquePaths(p.Instructions)
			p.Skills = uniquePaths(p.Skills)
			out.Agent = &a
		}
		if out.Stack != nil {
			for _, file := range out.Stack.Examples {
				if err := committed(file); err != nil {
					return nil, err
				}
			}
			out.Diagnostics = append(out.Diagnostics, "Stack commands and prerequisites are proposals. Select executable checks in project verification settings; no setup or commands run during input loading.")
			if _, exists := g.files["package.json"]; exists {
				data, err := g.read(ctx, "package.json")
				if err != nil {
					return nil, err
				}
				var pkg struct {
					PackageManager string            `json:"packageManager"`
					Engines        map[string]string `json:"engines"`
				}
				if json.Unmarshal(data, &pkg) != nil || len(pkg.PackageManager) > 256 || len(pkg.Engines) > 64 {
					return nil, fmt.Errorf("cannot reconcile stack profile with package.json")
				}
				detected := repoinspect.Inspect(g.regularFiles(), map[string][]byte{"package.json": data})
				if out.Stack.PackageManager != "" {
					manager, _, _ := strings.Cut(out.Stack.PackageManager, "@")
					if manager != detected.PackageManager {
						return nil, fmt.Errorf("stack %s requests %s but repository manifests/lockfiles select %s", out.Stack.Name, out.Stack.PackageManager, detected.PackageManager)
					}
				}
				if out.Stack.PackageManager != "" && pkg.PackageManager != "" && out.Stack.PackageManager != pkg.PackageManager {
					return nil, fmt.Errorf("stack %s requests %s but package.json pins %s", out.Stack.Name, out.Stack.PackageManager, pkg.PackageManager)
				}
				if err := add("package.json"); err != nil {
					return nil, err
				}
				if pkg.PackageManager != "" {
					out.Diagnostics = append(out.Diagnostics, "package.json packageManager: "+pkg.PackageManager)
				}
				for _, engine := range sortedKeys(pkg.Engines) {
					if len(engine) > 64 || len(pkg.Engines[engine]) > 256 {
						return nil, fmt.Errorf("package.json engine constraint exceeds supported bounds")
					}
					out.Diagnostics = append(out.Diagnostics, "package.json engines."+engine+": "+pkg.Engines[engine])
				}
			}
		}
		r.Profiles[name] = p
		resolved[name] = out
	}
	if len(resolved) == 0 {
		return nil, nil
	}
	return resolved, nil
}
func uniquePaths(paths []string) []string {
	out := []string{}
	for _, p := range paths {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
