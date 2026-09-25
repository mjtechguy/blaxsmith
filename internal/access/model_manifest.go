package access

import (
	_ "embed"
	"encoding/json"
	"slices"
	"strings"
)

// The bundled model manifest marks legacy models, badges, each provider's
// default model, and the reasoning efforts known models accept. Provider
// listings rarely say any of this, so the manifest overlays the stored list
// at read time; a value the provider did report wins.
//
// ponytail: bundled and versioned with the release. Upgrade path: fetch the
// same file from a remote URL (t3code's ModelManifest refreshes hourly with
// the bundle as fallback) once model churn outpaces releases.
//
//go:embed model_manifest.json
var manifestJSON []byte

type ManifestModel struct {
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Badge         string   `json:"badge"`
	Legacy        bool     `json:"legacy"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"defaultEffort"`
}

type manifestProvider struct {
	Default string          `json:"default"`
	Models  []ManifestModel `json:"models"`
}

var modelManifest = func() (m struct {
	Version   int                         `json:"version"`
	UpdatedAt string                      `json:"updatedAt"`
	Providers map[string]manifestProvider `json:"providers"`
}) {
	if err := json.Unmarshal(manifestJSON, &m); err != nil || m.Version != 1 {
		panic("invalid bundled model manifest")
	}
	return m
}()

// EffortOrder is the canonical low-to-high order of every known effort.
var EffortOrder = []string{"provider-default", "minimal", "low", "medium", "high", "xhigh", "max"}

// ManifestModels lists a provider's manifest models (current first, as listed).
func ManifestModels(provider string) []ManifestModel { return modelManifest.Providers[provider].Models }

// manifestEntry matches a model id exactly or as a dated snapshot of a slug
// (claude-opus-4-5-20251101 matches claude-opus-4-5).
func manifestEntry(provider, model string) (ManifestModel, bool) {
	for _, m := range modelManifest.Providers[provider].Models {
		if model == m.Slug || strings.HasPrefix(model, m.Slug+"-20") {
			return m, true
		}
	}
	return ManifestModel{}, false
}

// ModelMeta is one model's resolved presentation and effort metadata.
type ModelMeta struct {
	IsDefault, Legacy bool
	Badge             string
	Efforts           []string
	DefaultEffort     string
}

// ResolveModel overlays the manifest on what the provider reported. When
// harnessEfforts is non-nil, efforts are limited to what that harness
// accepts, and a default outside the result is dropped. Empty efforts mean
// "unknown": the caller offers the harness's own list.
func ResolveModel(provider, model string, reported ModelMeta, harnessEfforts []string) ModelMeta {
	out := reported
	if entry, ok := manifestEntry(provider, model); ok {
		out.Legacy = out.Legacy || entry.Legacy
		if out.Badge == "" {
			out.Badge = entry.Badge
		}
		if len(out.Efforts) == 0 {
			out.Efforts = entry.Efforts
		}
		if out.DefaultEffort == "" {
			out.DefaultEffort = entry.DefaultEffort
		}
		out.IsDefault = out.IsDefault || entry.Slug == modelManifest.Providers[provider].Default
	}
	out.Efforts = slices.Clone(out.Efforts)
	if harnessEfforts != nil && len(out.Efforts) > 0 {
		out.Efforts = slices.DeleteFunc(out.Efforts, func(e string) bool { return !slices.Contains(harnessEfforts, e) })
	}
	if !slices.Contains(out.Efforts, out.DefaultEffort) {
		out.DefaultEffort = ""
	}
	return out
}

// anthropicEfforts reads capabilities.effort from the Anthropic Models API:
// {"supported":true,"low":{"supported":true},…,"max":{"supported":true}}.
func anthropicEfforts(capabilities json.RawMessage) []string {
	var caps struct {
		Effort map[string]json.RawMessage `json:"effort"`
	}
	if len(capabilities) == 0 || json.Unmarshal(capabilities, &caps) != nil || caps.Effort == nil {
		return nil
	}
	var out []string
	for _, level := range EffortOrder {
		var leaf struct {
			Supported bool `json:"supported"`
		}
		if raw, ok := caps.Effort[level]; ok && json.Unmarshal(raw, &leaf) == nil && leaf.Supported {
			out = append(out, level)
		}
	}
	return out
}
