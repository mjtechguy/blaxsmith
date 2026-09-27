package anvil

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

const PlanSchema = "anvil.plan/v1alpha1"

type Decision struct {
	ID           string   `json:"id"`
	Question     string   `json:"question"`
	Choice       string   `json:"choice"`
	Reason       string   `json:"reason"`
	Authority    string   `json:"authority"` // user attribution or proposal; never platform authority.
	Sources      []string `json:"sources"`
	Alternatives []string `json:"alternatives,omitempty"`
}
type Unknown struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Disposition string   `json:"disposition"` // blocking, investigable, assumed, deferred.
	Reason      string   `json:"reason"`
	Sources     []string `json:"sources"`
}
type Plan struct {
	Decisions     []Decision    `json:"decisions,omitempty"`
	Unknowns      []Unknown     `json:"unknowns,omitempty"`
	Schema        string        `json:"schema_version"`
	Title         string        `json:"title"`
	Summary       string        `json:"summary"`
	Assumptions   []string      `json:"assumptions"`
	OutOfScope    []string      `json:"out_of_scope"`
	OpenQuestions []string      `json:"open_questions"`
	Requirements  []Requirement `json:"requirements"`
	Phases        []Phase       `json:"phases"`
	Tasks         []Task        `json:"tasks"`
}
type Requirement struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Sources     []string `json:"sources"`
	Examples    []string `json:"examples"`
}
type Phase struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Outcome string `json:"outcome"`
}
type Task struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Phase          string   `json:"phase"`
	Reason         string   `json:"reason"`
	DependsOn      []string `json:"depends_on"`
	RequirementIDs []string `json:"requirement_ids"`
	Instructions   string   `json:"instructions"`
	Acceptance     []string `json:"acceptance"`
	Validation     []string `json:"validation"`
}

var planID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

func planText(s string, max int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= max && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}
func planStrings(values []string, min, max int) bool {
	if len(values) < min || len(values) > max {
		return false
	}
	for _, v := range values {
		if !planText(v, 4000) {
			return false
		}
	}
	return true
}

// ValidatePlan checks references, coverage and graph structure, never code
// correctness or authorization. The returned JSON is canonical and bounded.
func ValidatePlan(data []byte, references map[string]bool) (Plan, []byte, error) {
	var p Plan
	if len(data) == 0 || len(data) > 256<<10 {
		return p, nil, fmt.Errorf("plan must contain 1–262144 bytes")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return p, nil, fmt.Errorf("plan must be one JSON object with supported fields")
	}
	if p.Schema != PlanSchema || !planText(p.Title, 300) || !planText(p.Summary, 8000) || !planStrings(p.Assumptions, 0, 32) || !planStrings(p.OutOfScope, 0, 32) || !planStrings(p.OpenQuestions, 0, 32) || len(p.Requirements) < 1 || len(p.Requirements) > 128 || len(p.Phases) < 1 || len(p.Phases) > 16 || len(p.Tasks) < 1 || len(p.Tasks) > 64 {
		return p, nil, fmt.Errorf("plan needs a title, summary, 1–128 requirements, 1–16 phases and 1–64 tasks")
	}
	if len(p.Decisions) > 32 || len(p.Unknowns) > 32 {
		return p, nil, fmt.Errorf("plan permits at most 32 decisions and 32 classified unknowns")
	}
	decisionIDs, questions, unknownIDs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	validSources := func(sources []string) bool {
		if !planStrings(sources, 1, 32) {
			return false
		}
		for _, ref := range sources {
			if !references[ref] {
				return false
			}
		}
		return true
	}
	for _, v := range p.Decisions {
		question := strings.ToLower(strings.Join(strings.Fields(v.Question), " "))
		if !planID.MatchString(v.ID) || decisionIDs[v.ID] || questions[question] || !planText(v.Question, 4000) || !planText(v.Choice, 4000) || !planText(v.Reason, 4000) || (v.Authority != "user" && v.Authority != "proposal") || !validSources(v.Sources) || !planStrings(v.Alternatives, 0, 8) {
			return p, nil, fmt.Errorf("decision %s has invalid sources, fields, or a repeated question; reconcile conflicting decisions", v.ID)
		}
		decisionIDs[v.ID] = true
		questions[question] = true
	}
	for _, v := range p.Unknowns {
		if !planID.MatchString(v.ID) || unknownIDs[v.ID] || !planText(v.Description, 4000) || !planText(v.Reason, 4000) || (v.Disposition != "blocking" && v.Disposition != "investigable" && v.Disposition != "assumed" && v.Disposition != "deferred") || !validSources(v.Sources) {
			return p, nil, fmt.Errorf("unknown %s needs a classification, reason, and saved sources", v.ID)
		}
		unknownIDs[v.ID] = true
	}
	requirements, phases, tasks, covered, usedPhase := map[string]bool{}, map[string]bool{}, map[string]Task{}, map[string]bool{}, map[string]bool{}
	for _, r := range p.Requirements {
		if !planID.MatchString(r.ID) || requirements[r.ID] || !planText(r.Description, 4000) || !planStrings(r.Sources, 1, 32) || !planStrings(r.Examples, 1, 16) {
			return p, nil, fmt.Errorf("invalid or duplicate requirement")
		}
		for _, ref := range r.Sources {
			if !references[ref] {
				return p, nil, fmt.Errorf("requirement %s cites an unknown source", r.ID)
			}
		}
		requirements[r.ID] = true
	}
	for _, phase := range p.Phases {
		if !planID.MatchString(phase.ID) || phases[phase.ID] || !planText(phase.Title, 300) || !planText(phase.Outcome, 4000) {
			return p, nil, fmt.Errorf("invalid or duplicate phase")
		}
		phases[phase.ID] = true
	}
	for _, t := range p.Tasks {
		if !planID.MatchString(t.ID) || tasks[t.ID].ID != "" || !phases[t.Phase] || !planText(t.Title, 300) || !planText(t.Reason, 4000) || !planText(t.Instructions, 16000) || !planStrings(t.Acceptance, 1, 16) || !planStrings(t.Validation, 0, 16) || !planStrings(t.RequirementIDs, 1, 32) || len(t.DependsOn) > 64 {
			return p, nil, fmt.Errorf("invalid task, phase, or completion criteria")
		}
		for _, r := range t.RequirementIDs {
			if !requirements[r] {
				return p, nil, fmt.Errorf("task %s cites an unknown requirement", t.ID)
			}
			covered[r] = true
		}
		tasks[t.ID] = t
		usedPhase[t.Phase] = true
	}
	for id := range requirements {
		if !covered[id] {
			return p, nil, fmt.Errorf("requirement %s has no implementing task", id)
		}
	}
	for id := range phases {
		if !usedPhase[id] {
			return p, nil, fmt.Errorf("phase %s has no tasks", id)
		}
	}
	state := map[string]int{}
	var visit func(string) error
	visit = func(id string) error {
		if state[id] == 1 {
			return fmt.Errorf("task dependency cycle at %s", id)
		}
		if state[id] == 2 {
			return nil
		}
		t, ok := tasks[id]
		if !ok {
			return fmt.Errorf("unknown task dependency")
		}
		state[id] = 1
		seen := map[string]bool{}
		for _, dep := range t.DependsOn {
			if seen[dep] {
				return fmt.Errorf("task %s repeats a dependency", id)
			}
			seen[dep] = true
			if err := visit(dep); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, t := range p.Tasks {
		if err := visit(t.ID); err != nil {
			return p, nil, err
		}
	}
	canonical, err := json.Marshal(p)
	if len(canonical) > 256<<10 {
		return p, nil, fmt.Errorf("plan exceeds 256 KiB")
	}
	return p, canonical, err
}
