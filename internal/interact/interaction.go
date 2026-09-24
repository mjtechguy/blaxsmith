// Package interact persists structured human interactions (questions,
// approvals, escalations, interview rounds) raised by guests through `bx` or by
// the platform, delivers answers and steering back, and renders interview
// transcripts. See docs/interactive-sessions.md.
package interact

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

var keyPattern = regexp.MustCompile(`^[0-9A-Za-z_-]{1,64}$`)
var tagPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_:]{0,63}$`)

const (
	MaxAnswerText = 4000
	maxOptions    = 20
)

type Option struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

type Source struct {
	Path string `json:"path"`
	Line int    `json:"line,omitempty"`
}

type Interview struct {
	Round          int    `json:"round"`
	FinalizeOption string `json:"finalize_option,omitempty"`
	// Tags are optional Forge transcript tags (ARCH_INVARIANT, IMPLICIT_FACT:RUNTIME).
	Tags []string `json:"tags,omitempty"`
}

// Interaction is the contract JSON shape shared with the guest shim.
type Interaction struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Title         string     `json:"title"`
	BodyMD        string     `json:"body_md,omitempty"`
	Options       []Option   `json:"options,omitempty"`
	MultiSelect   bool       `json:"multi_select,omitempty"`
	AllowFreeText bool       `json:"allow_free_text,omitempty"`
	Blocking      bool       `json:"blocking,omitempty"`
	Sources       []Source   `json:"sources,omitempty"`
	Interview     *Interview `json:"interview,omitempty"`
}

type Answer struct {
	InteractionID string    `json:"interaction_id"`
	OptionIDs     []string  `json:"option_ids"`
	Text          string    `json:"text,omitempty"`
	AnsweredBy    string    `json:"answered_by"`
	At            time.Time `json:"at"`
}

// Record is a persisted interaction as the browser sees it.
type Record struct {
	ID        string
	RunID     string
	AttemptID string // empty for platform escalations
	Stage     string
	State     string
	Interaction
	Answer    *Answer
	CreatedAt time.Time
}

func runes(s string, lo, hi int) bool {
	n := utf8.RuneCountInString(s)
	return utf8.ValidString(s) && n >= lo && n <= hi
}

// validate normalizes and checks an interaction; it never trusts guest size.
func (ix *Interaction) validate() error {
	ix.Title, ix.BodyMD = strings.TrimSpace(ix.Title), strings.TrimSpace(ix.BodyMD)
	if !keyPattern.MatchString(ix.ID) || !slices.Contains([]string{"question", "approval", "escalation", "interview_round"}, ix.Kind) ||
		!runes(ix.Title, 1, 300) || !runes(ix.BodyMD, 0, 32000) || len(ix.Options) > maxOptions || len(ix.Sources) > 50 ||
		(len(ix.Options) == 0 && !ix.AllowFreeText) {
		return workflow.ErrInvalid
	}
	seen := map[string]bool{}
	for i := range ix.Options {
		o := &ix.Options[i]
		o.Label, o.Description = strings.TrimSpace(o.Label), strings.TrimSpace(o.Description)
		if !keyPattern.MatchString(o.ID) || seen[o.ID] || !runes(o.Label, 1, 200) || !runes(o.Description, 0, 1000) {
			return workflow.ErrInvalid
		}
		seen[o.ID] = true
	}
	for _, s := range ix.Sources {
		if !runes(s.Path, 1, 1024) || s.Line < 0 {
			return workflow.ErrInvalid
		}
	}
	if iv := ix.Interview; iv != nil {
		if iv.Round < 0 || iv.Round > 1000 || (iv.FinalizeOption != "" && !seen[iv.FinalizeOption]) || len(iv.Tags) > 8 {
			return workflow.ErrInvalid
		}
		for _, tag := range iv.Tags {
			if !tagPattern.MatchString(tag) {
				return workflow.ErrInvalid
			}
		}
	}
	return nil
}

// checkAnswer validates a browser answer against the stored interaction.
func (ix Interaction) checkAnswer(optionIDs []string, text string) (string, error) {
	text = strings.TrimSpace(text)
	if !runes(text, 0, MaxAnswerText) || len(optionIDs) > maxOptions ||
		(text != "" && !ix.AllowFreeText) || (len(optionIDs) == 0 && text == "") ||
		(!ix.MultiSelect && len(optionIDs) > 1) {
		return "", workflow.ErrInvalid
	}
	seen := map[string]bool{}
	for _, id := range optionIDs {
		if seen[id] || !slices.ContainsFunc(ix.Options, func(o Option) bool { return o.ID == id }) {
			return "", workflow.ErrInvalid
		}
		seen[id] = true
	}
	return text, nil
}

// progressPayload keeps a `bx event` object, truncating text rather than
// rejecting, so it fits the 8 KiB workflow_events payload cap.
func progressPayload(raw json.RawMessage) ([]byte, error) {
	var body map[string]any
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, workflow.ErrInvalid
	}
	kind, _ := body["type"].(string)
	if !slices.Contains([]string{"phase", "cycle", "handoff", "finding", "progress"}, kind) {
		return nil, workflow.ErrInvalid
	}
	for _, limit := range []int{2000, 500, 120} {
		for key, value := range body {
			if s, ok := value.(string); ok && utf8.RuneCountInString(s) > limit {
				body[key] = string([]rune(s)[:limit]) + "…"
			}
		}
		data, err := json.Marshal(body)
		if err == nil && len(data) <= 8192 {
			return data, nil
		}
	}
	// Still too large (deep nesting or many keys): keep only the settled fields.
	kept := map[string]any{}
	for _, key := range []string{"type", "cycle", "max_cycles", "status", "text", "message", "name"} {
		if value, ok := body[key]; ok {
			if _, nested := value.(map[string]any); !nested {
				kept[key] = value
			}
		}
	}
	data, err := json.Marshal(kept)
	if err != nil || len(data) > 8192 {
		return []byte(`{"type":"` + kind + `"}`), nil
	}
	return data, nil
}
