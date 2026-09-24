package access

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestResolveModelManifestEfforts(t *testing.T) {
	// Manifest efforts and default, limited to what the harness accepts.
	m := ResolveModel("anthropic", "claude-opus-4-6", ModelMeta{}, []string{"low", "medium", "high", "xhigh", "max"})
	if !m.Legacy || !slices.Equal(m.Efforts, []string{"low", "medium", "high", "max"}) || m.DefaultEffort != "high" {
		t.Fatalf("opus 4.6: %+v", m)
	}
	// A dated snapshot matches its slug.
	if m := ResolveModel("anthropic", "claude-opus-4-5-20251101", ModelMeta{}, nil); !m.Legacy || len(m.Efforts) != 3 {
		t.Fatalf("dated snapshot: %+v", m)
	}
	// Provider-reported efforts win over the manifest; the default is dropped
	// when the harness filter removes it.
	m = ResolveModel("anthropic", "claude-opus-5-5", ModelMeta{Efforts: []string{"low", "high"}}, []string{"high", "max"})
	if !slices.Equal(m.Efforts, []string{"high"}) || m.DefaultEffort != "" || m.Badge != "new" {
		t.Fatalf("reported efforts: %+v", m)
	}
	if m := ResolveModel("anthropic", "claude-opus-5", ModelMeta{}, nil); !m.IsDefault || m.DefaultEffort != "high" || m.Legacy {
		t.Fatalf("default model: %+v", m)
	}
	// Codex accepts "minimal"; the Claude Code harness does not.
	if m := ResolveModel("openai", "gpt-5", ModelMeta{}, []string{"low", "medium", "high", "xhigh", "max"}); slices.Contains(m.Efforts, "minimal") {
		t.Fatalf("harness filter kept minimal: %+v", m)
	}
	// Unknown models have no efforts: callers fall back to the harness list.
	if m := ResolveModel("opencode", "grok-code", ModelMeta{}, []string{"low"}); len(m.Efforts) != 0 || m.DefaultEffort != "" {
		t.Fatalf("unknown model: %+v", m)
	}
	if m := ResolveModel("anthropic", "claude-haiku-4-5", ModelMeta{}, nil); len(m.Efforts) != 0 || !m.Legacy {
		t.Fatalf("haiku: %+v", m)
	}
}

func TestAnthropicCapabilityEfforts(t *testing.T) {
	caps := json.RawMessage(`{"effort":{"supported":true,"max":{"supported":true},"low":{"supported":true},"medium":{"supported":false},"high":{"supported":true}},"thinking":{"supported":true}}`)
	if got := anthropicEfforts(caps); !slices.Equal(got, []string{"low", "high", "max"}) {
		t.Fatalf("efforts %v", got)
	}
	if got := anthropicEfforts(json.RawMessage(`{"thinking":{}}`)); got != nil {
		t.Fatalf("no effort capability: %v", got)
	}
}
