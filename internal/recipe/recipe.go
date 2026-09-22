// Package recipe validates Git-authored engineering workflows. It does not
// authorize access, approve changes, or launch workers.
package recipe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const Schema = "blaxsmith.recipe/v1alpha1"

type Recipe struct {
	SchemaVersion  string             `json:"schema_version"`
	Name           string             `json:"name"`
	Profiles       map[string]Profile `json:"profiles"`
	Stages         []Stage            `json:"stages"`
	RequiredChecks []string           `json:"required_checks"`
	Limits         Limits             `json:"limits"`
}

type Profile struct {
	Harness      string   `json:"harness"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Instructions []string `json:"instructions,omitempty"`
	Skills       []string `json:"skills,omitempty"`
}

type Stage struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Profile   string   `json:"profile,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	Prompt    string   `json:"prompt,omitempty"`
}

type Limits struct {
	MaxCorrectionCycles int `json:"max_correction_cycles"`
	TimeoutSeconds      int `json:"timeout_seconds"`
}

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func parse(data []byte) (Recipe, error) {
	var r Recipe
	// encoding/json otherwise silently accepts duplicate keys, including profiles.
	if err := uniqueKeys(json.NewDecoder(bytes.NewReader(data))); err != nil {
		return r, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&r); err != nil {
		return r, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return r, fmt.Errorf("expected one JSON document")
	}
	return r, nil
}

func uniqueKeys(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for d.More() {
		if delim == '{' {
			key, err := d.Token()
			if err != nil {
				return err
			}
			k := strings.ToLower(key.(string))
			if seen[k] {
				return fmt.Errorf("duplicate JSON key %q", key)
			}
			seen[k] = true
		}
		if err := uniqueKeys(d); err != nil {
			return err
		}
	}
	_, err = d.Token()
	return err
}

// validate returns a stable topological order, using declaration order for ties.
func (r Recipe) validate() ([]string, error) {
	if r.SchemaVersion != Schema || !identifier.MatchString(r.Name) {
		return nil, fmt.Errorf("recipe requires schema_version %q and a valid name", Schema)
	}
	if r.Limits.MaxCorrectionCycles < 1 || r.Limits.MaxCorrectionCycles > 10 || r.Limits.TimeoutSeconds < 1 || r.Limits.TimeoutSeconds > 86400 {
		return nil, fmt.Errorf("limits require 1–10 correction cycles and 1–86400 seconds")
	}
	if len(r.RequiredChecks) == 0 || len(r.Profiles) == 0 || len(r.Stages) > 64 {
		return nil, fmt.Errorf("require checks, profiles, and at most 64 stages")
	}
	seenChecks := map[string]bool{}
	for _, check := range r.RequiredChecks {
		if !identifier.MatchString(check) || seenChecks[check] {
			return nil, fmt.Errorf("invalid or duplicate required check %q", check)
		}
		seenChecks[check] = true
	}
	for _, name := range sortedKeys(r.Profiles) {
		p := r.Profiles[name]
		if !identifier.MatchString(name) || (p.Harness != "claude-code" && p.Harness != "codex" && p.Harness != "opencode") || strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Effort) == "" {
			return nil, fmt.Errorf("profile %q requires an explicit Claude Code/Codex/OpenCode harness, model, and effort", name)
		}
		if p.Harness == "opencode" {
			provider, model, ok := strings.Cut(p.Model, "/")
			if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
				return nil, fmt.Errorf("OpenCode profile %q requires a provider/model identifier", name)
			}
		}
		for _, file := range append(slices.Clone(p.Instructions), p.Skills...) {
			if !validPath(file) {
				return nil, fmt.Errorf("profile %q has invalid file path %q", name, file)
			}
		}
	}
	stages := map[string]Stage{}
	kinds := map[string][]string{}
	for _, s := range r.Stages {
		if !identifier.MatchString(s.ID) || stages[s.ID].ID != "" {
			return nil, fmt.Errorf("invalid or duplicate stage ID %q", s.ID)
		}
		switch s.Kind {
		case "plan", "implement", "review", "verify", "architect_review", "research", "integrate", "ui_review", "documentation":
			if _, ok := r.Profiles[s.Profile]; !ok || !validPath(s.Prompt) {
				return nil, fmt.Errorf("stage %q requires a known profile and prompt file", s.ID)
			}
		case "human_review":
			if s.Profile != "" || s.Prompt != "" {
				return nil, fmt.Errorf("human review %q cannot have an agent profile or prompt", s.ID)
			}
		default:
			return nil, fmt.Errorf("stage %q has unsupported kind %q", s.ID, s.Kind)
		}
		stages[s.ID] = s
		kinds[s.Kind] = append(kinds[s.Kind], s.ID)
	}
	for _, k := range []string{"plan", "implement", "review", "verify", "architect_review", "human_review"} {
		if len(kinds[k]) == 0 {
			return nil, fmt.Errorf("missing mandatory %s stage", k)
		}
	}
	if len(kinds["human_review"]) != 1 || len(kinds["architect_review"]) != 1 {
		return nil, fmt.Errorf("require exactly one final architect review and one human review")
	}
	for _, s := range r.Stages {
		seen := map[string]bool{}
		for _, dep := range s.DependsOn {
			if _, ok := stages[dep]; !ok || dep == s.ID || seen[dep] {
				return nil, fmt.Errorf("stage %q has invalid or duplicate dependency %q", s.ID, dep)
			}
			seen[dep] = true
		}
	}
	order := []string{}
	ancestors := map[string]map[string]bool{}
	for len(order) < len(r.Stages) {
		before := len(order)
		for _, s := range r.Stages {
			if ancestors[s.ID] != nil {
				continue
			}
			a := map[string]bool{}
			ready := true
			for _, dep := range s.DependsOn {
				if ancestors[dep] == nil {
					ready = false
					break
				}
				a[dep] = true
				for ancestor := range ancestors[dep] {
					a[ancestor] = true
				}
			}
			if ready {
				order = append(order, s.ID)
				ancestors[s.ID] = a
			}
		}
		if len(order) == before {
			return nil, fmt.Errorf("stage dependencies contain a cycle")
		}
	}
	human, architect := kinds["human_review"][0], kinds["architect_review"][0]
	for _, s := range r.Stages {
		if s.ID != human && !ancestors[human][s.ID] {
			return nil, fmt.Errorf("stage %q must precede final human review", s.ID)
		}
		if s.ID != human && s.ID != architect && !ancestors[architect][s.ID] {
			return nil, fmt.Errorf("stage %q must precede final architect review", s.ID)
		}
		if s.Kind == "implement" {
			planned, reviewed, verified := false, false, false
			for _, id := range kinds["plan"] {
				planned = planned || ancestors[s.ID][id]
			}
			for _, id := range kinds["review"] {
				reviewed = reviewed || ancestors[id][s.ID]
			}
			for _, id := range kinds["verify"] {
				verified = verified || ancestors[id][s.ID]
			}
			if !planned || !reviewed || !verified {
				return nil, fmt.Errorf("implementation %q needs prior planning and subsequent review and verification", s.ID)
			}
		}
	}
	return order, nil
}

func validPath(p string) bool {
	return p != "" && p != "." && !strings.HasPrefix(p, "/") && p != ".." && !strings.HasPrefix(p, "../") && path.Clean(p) == p && utf8.ValidString(p) && !strings.Contains(p, "\\") && !strings.ContainsFunc(p, unicode.IsControl)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
