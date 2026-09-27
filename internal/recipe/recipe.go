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

	"github.com/mjtechguy/blaxsmith/internal/extension"
)

const Schema = "blaxsmith.recipe/v1alpha1"

type Recipe struct {
	SchemaVersion  string             `json:"schema_version"`
	Name           string             `json:"name"`
	Profiles       map[string]Profile `json:"profiles"`
	Stages         []Stage            `json:"stages"`
	RequiredChecks []string           `json:"required_checks"`
	Limits         Limits             `json:"limits"`
	// Acceptance selects human review or acceptance by the frozen policy.
	Acceptance string      `json:"acceptance,omitempty"`
	Factory    *Factory    `json:"factory,omitempty"`
	Documents  []string    `json:"documents,omitempty"`
	Validation *Validation `json:"validation,omitempty"`
}

// Validation selects an explicitly installed preflight validator and its named files.
type Validation struct {
	ID     string            `json:"id"`
	Inputs map[string]string `json:"inputs"`
}

// Factory is attribution, never authorization or an executable plugin selector.
type Factory struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

var connectionID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

type Profile struct {
	Connection   string   `json:"connection,omitempty"` // Optional exact billing connection; no fallback.
	Inputs       string   `json:"inputs,omitempty"`     // Committed stack/rules/agent definitions.
	Agent        string   `json:"agent,omitempty"`      // Definition ID in inputs; grants no authority.
	Harness      string   `json:"harness"`
	Model        string   `json:"model"`
	Effort       string   `json:"effort"`
	Instructions []string `json:"instructions,omitempty"`
	Skills       []string `json:"skills,omitempty"`
}

type Stage struct {
	ReviewReport string   `json:"review_report,omitempty"` // Artifact ID containing blaxsmith.review/v1alpha1.
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Profile      string   `json:"profile,omitempty"`
	DependsOn    []string `json:"depends_on,omitempty"`
	Prompt       string   `json:"prompt,omitempty"`
	Loop         *Loop    `json:"loop,omitempty"`
	// Template names an installed extension stage template,
	// "extension@version/template" (docs/extensions-and-runtimes.md). Freeze
	// resolves it to one installed version and records its digests.
	Template string `json:"template,omitempty"`
	Mode     string `json:"mode,omitempty"` // Review verdict: required (default) or advisory. Omit a stage to turn it off.
}

// Loop reruns a review/verify stage after a correction attempt on With until
// it reports a pass. MaxCycles bounds corrections before human escalation.
type Loop struct {
	With      string `json:"with"`
	Until     string `json:"until"`
	MaxCycles int    `json:"max_cycles"`
}

// Limits bound a stage. TimeoutSeconds is an IDLE timeout: a stage fails only
// after that long with no progress (harness output, a `bx event`, or pane
// activity). MaxRuntimeSeconds is an optional total cap; 0 means unlimited.
type Limits struct {
	MaxCorrectionCycles int `json:"max_correction_cycles"`
	TimeoutSeconds      int `json:"timeout_seconds"`
	MaxRuntimeSeconds   int `json:"max_runtime_seconds,omitempty"`
}

// MaxRuntimeCap is the longest total stage runtime a recipe may request.
const MaxRuntimeCap = 7 * 24 * 3600

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
		path := "schema_version"
		if r.SchemaVersion == Schema {
			path = "name"
		}
		return nil, fieldErr(path, "recipe requires schema_version %q and a valid name", Schema)
	}
	if r.Acceptance != "manual" && r.Acceptance != "policy" {
		return nil, fieldErr("acceptance", "recipes require explicit manual or policy acceptance")
	}
	if r.Factory != nil && (!identifier.MatchString(r.Factory.ID) || strings.TrimSpace(r.Factory.Version) == "" || len(r.Factory.Version) > 128) {
		return nil, fieldErr("factory", "factory requires a valid id and a version of 1–128 bytes")
	}
	for i, file := range r.Documents {
		if !validPath(file) {
			return nil, fieldErr(fmt.Sprintf("documents[%d]", i), "invalid document path")
		}
	}
	if r.Validation != nil {
		if !identifier.MatchString(r.Validation.ID) || len(r.Validation.Inputs) == 0 || len(r.Validation.Inputs) > 32 {
			return nil, fieldErr("validation", "validation requires an installed validator id and 1–32 named inputs")
		}
		for name, file := range r.Validation.Inputs {
			if !identifier.MatchString(name) || !validPath(file) {
				return nil, fieldErr("validation.inputs", "invalid validator input name or file path")
			}
		}
	}
	if r.Limits.MaxCorrectionCycles < 0 || r.Limits.MaxCorrectionCycles > 10 || r.Limits.TimeoutSeconds < 1 || r.Limits.TimeoutSeconds > 86400 ||
		r.Limits.MaxRuntimeSeconds < 0 || r.Limits.MaxRuntimeSeconds > MaxRuntimeCap {
		return nil, fieldErr("limits", "limits require 0–10 correction cycles, a 1–86400 second idle timeout, and a 0–%d second max runtime", MaxRuntimeCap)
	}
	if len(r.Profiles) == 0 || len(r.Profiles) > 64 || len(r.Stages) == 0 || len(r.Stages) > 64 {
		path := "stages"
		if len(r.Profiles) == 0 {
			path = "profiles"
		}
		return nil, fieldErr(path, "require 1–64 profiles and 1–64 stages")
	}
	seenChecks := map[string]bool{}
	for i, check := range r.RequiredChecks {
		if !identifier.MatchString(check) || seenChecks[check] {
			return nil, fieldErr(fmt.Sprintf("required_checks[%d]", i), "invalid or duplicate required check %q", check)
		}
		seenChecks[check] = true
	}
	for _, name := range sortedKeys(r.Profiles) {
		p := r.Profiles[name]
		if p.Connection != "" && (len(p.Connection) > 64 || !connectionID.MatchString(p.Connection)) {
			return nil, fieldErr("profiles."+name+".connection", "connection must be an exact connection ID")
		}
		if (p.Inputs != "" && !validPath(p.Inputs)) || (p.Agent != "" && (p.Inputs == "" || !identifier.MatchString(p.Agent))) {
			return nil, fieldErr("profiles."+name, "agent requires an inputs file and a valid definition ID")
		}
		if !identifier.MatchString(name) || (p.Harness != "claude-code" && p.Harness != "codex" && p.Harness != "opencode") || strings.TrimSpace(p.Model) == "" || strings.TrimSpace(p.Effort) == "" {
			return nil, fieldErr("profiles."+name, "profile %q requires an explicit Claude Code/Codex/OpenCode harness, model, and effort", name)
		}
		if p.Harness == "opencode" {
			provider, model, ok := strings.Cut(p.Model, "/")
			if !ok || strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" {
				return nil, fieldErr("profiles."+name+".model", "OpenCode profile %q requires a provider/model identifier", name)
			}
		}
		for i, file := range append(slices.Clone(p.Instructions), p.Skills...) {
			if !validPath(file) {
				field := fmt.Sprintf("profiles.%s.instructions[%d]", name, i)
				if i >= len(p.Instructions) {
					field = fmt.Sprintf("profiles.%s.skills[%d]", name, i-len(p.Instructions))
				}
				return nil, fieldErr(field, "profile %q has invalid file path %q", name, file)
			}
		}
	}
	stages := map[string]Stage{}
	kinds := map[string][]string{}
	index := map[string]int{}
	for i, s := range r.Stages {
		at := fmt.Sprintf("stages[%d]", i)
		if s.ReviewReport != "" && (!identifier.MatchString(s.ReviewReport) || (s.Kind != "review" && s.Kind != "architect_review" && s.Kind != "ui_review")) {
			return nil, fieldErr(at+".review_report", "review_report needs a review stage and a valid artifact ID")
		}
		if s.Mode != "" && ((s.Mode != "required" && s.Mode != "advisory") || (s.Kind != "review" && s.Kind != "architect_review" && s.Kind != "ui_review")) {
			return nil, fieldErr(at+".mode", "mode requires a review stage and must be required or advisory")
		}
		if s.Mode == "advisory" && s.Loop != nil {
			return nil, fieldErr(at+".loop", "advisory reviews cannot require correction loops")
		}
		if !identifier.MatchString(s.ID) || stages[s.ID].ID != "" {
			return nil, fieldErr(at+".id", "invalid or duplicate stage ID %q", s.ID)
		}
		switch s.Kind {
		case "plan", "interview", "implement", "review", "verify", "architect_review", "research", "integrate", "ui_review", "documentation":
			if _, ok := r.Profiles[s.Profile]; !ok || !validPath(s.Prompt) {
				field := at + ".prompt"
				if _, ok := r.Profiles[s.Profile]; !ok {
					field = at + ".profile"
				}
				return nil, fieldErr(field, "stage %q requires a known profile and prompt file", s.ID)
			}
		case "human_review":
			if s.Profile != "" || s.Prompt != "" || s.Template != "" {
				return nil, fieldErr(at, "human review %q cannot have an agent profile or prompt", s.ID)
			}
		default:
			return nil, fieldErr(at+".kind", "stage %q has unsupported kind %q", s.ID, s.Kind)
		}
		if _, _, _, ok := extension.ParseTemplateRef(s.Template); s.Template != "" && !ok {
			return nil, fieldErr(at+".template", "stage %q template must be extension@version/template", s.ID)
		}
		if s.Loop != nil && (s.Kind != "review" && s.Kind != "verify" || s.Loop.Until != "pass" ||
			s.Loop.MaxCycles < 1 || s.Loop.MaxCycles > 10) {
			return nil, fieldErr(at+".loop", "stage %q loop needs a review/verify stage, until \"pass\", and 1–10 cycles", s.ID)
		}
		stages[s.ID] = s
		index[s.ID] = i
		kind := s.Kind
		if kind == "interview" {
			kind = "plan" // An interview is a plan the architect runs with a human.
		}
		kinds[kind] = append(kinds[kind], s.ID)
	}
	if len(kinds["human_review"]) > 1 || (r.Acceptance == "policy" && len(kinds["human_review"]) != 0) || len(kinds["human_review"]) == len(r.Stages) {
		return nil, fieldErr("stages", "workflows need an agent stage and at most one final human review, only with manual acceptance")
	}
	// The run branch transport currently owns a single implementation stream.
	// Refuse graphs it cannot merge rather than silently discarding a branch.
	if len(kinds["implement"]) > 1 {
		return nil, fieldErr("stages", "the current run-branch capability supports one implement stage per run")
	}

	for i, s := range r.Stages {
		seen := map[string]bool{}
		for j, dep := range s.DependsOn {
			if _, ok := stages[dep]; !ok || dep == s.ID || seen[dep] {
				return nil, fieldErr(fmt.Sprintf("stages[%d].depends_on[%d]", i, j), "stage %q has invalid or duplicate dependency %q", s.ID, dep)
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
			return nil, fieldErr("stages", "stage dependencies contain a cycle")
		}
	}
	human := ""
	if len(kinds["human_review"]) > 0 {
		human = kinds["human_review"][0]
	}

	for _, s := range r.Stages {
		if s.Loop != nil && (!ancestors[s.ID][s.Loop.With] || stages[s.Loop.With].Kind == "human_review") {
			return nil, fieldErr(stagePath(index, s.ID)+".loop.with", "stage %q loop target %q must be an upstream agent stage", s.ID, s.Loop.With)
		}
		if human != "" && s.ID != human && !ancestors[human][s.ID] {
			return nil, fieldErr(stagePath(index, s.ID)+".depends_on", "stage %q must precede final human review", s.ID)
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
