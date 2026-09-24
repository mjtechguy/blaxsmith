package extension

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/extension/fixture"
)

func guildManifest(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("../../examples/extensions/guild/blaxsmith-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutate decodes the Guild manifest, applies change, and re-encodes it.
func mutate(t *testing.T, change func(map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(guildManifest(t), &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func templates(doc map[string]any) []any { return doc["stage_templates"].([]any) }

func TestGuildManifestIsValidAgainstTheGuildLayout(t *testing.T) {
	m, perr := Parse(guildManifest(t))
	if perr != nil {
		t.Fatal(perr)
	}
	if m.ID != "guild" || m.Version != "1.0.0" || len(m.StageTemplates) != 2 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	foundry, ok := m.Template("foundry-build")
	if !ok || foundry.Harness != "claude-code" || !slices.Equal(foundry.Plugins, []string{"foundry"}) || len(foundry.Signals) != 2 {
		t.Fatalf("foundry template: %+v", foundry)
	}
	dir, commit := fixture.Repo(t, fixture.GuildFiles)
	tree, err := OpenGitTree(t.Context(), dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateTree(m, tree); err != nil {
		t.Fatal(err)
	}
	// The real Guild checkout, when a test run provides one.
	if repo := os.Getenv("BLAXSMITH_GUILD_REPO"); repo != "" {
		real, err := OpenGitTree(t.Context(), repo, "5f147996c9ff80bebba9b9ab121a9c639a008d69")
		if err != nil {
			t.Fatal(err)
		}
		if err := ValidateTree(m, real); err != nil {
			t.Fatalf("Guild at 5f14799: %v", err)
		}
	}
	var ids []string
	for _, p := range m.Permissions() {
		ids = append(ids, p.ID)
	}
	want := []string{"subagents", "mcp:foundry", "mcp:serena", "hook:foundry-serena", "egress:pypi.org",
		"egress:files.pythonhosted.org", "egress:github.com", "egress:objects.githubusercontent.com",
		"egress:registry.npmjs.org", "runtime:uv", "runtime:python", "runtime:serena"}
	if !slices.Equal(ids, want) {
		t.Fatalf("permissions %v", ids)
	}
}

func TestManifestRejectsBadInput(t *testing.T) {
	cases := map[string]struct {
		data []byte
		path string
	}{
		"unknown field":   {mutate(t, func(d map[string]any) { d["marketplace"] = true }), "$"},
		"schema":          {mutate(t, func(d map[string]any) { d["schema_version"] = "v2" }), "schema_version"},
		"bad id":          {mutate(t, func(d map[string]any) { d["id"] = "Guild" }), "id"},
		"bad version":     {mutate(t, func(d map[string]any) { d["version"] = "1.0" }), "version"},
		"unknown harness": {mutate(t, func(d map[string]any) { d["harnesses"].([]any)[0].(map[string]any)["harness"] = "grok" }), "harnesses[0].harness"},
		"escaping plugin": {mutate(t, func(d map[string]any) { d["plugins"].([]any)[0].(map[string]any)["path"] = "../forge" }), "plugins[0]"},
		"egress port":     {mutate(t, func(d map[string]any) { d["egress"].([]any)[0].(map[string]any)["port"] = 80 }), "egress[0]"},
		"remote mcp": {mutate(t, func(d map[string]any) {
			d["mcp_servers"].([]any)[1].(map[string]any)["url"] = "https://mcp.example.com/mcp"
		}), "mcp_servers[1].url"},
		"hook plugin": {mutate(t, func(d map[string]any) { d["hooks"].([]any)[0].(map[string]any)["plugin"] = "crew" }), "hooks[0]"},
		"template plugin": {mutate(t, func(d map[string]any) {
			templates(d)[0].(map[string]any)["plugins"] = []any{"crew"}
		}), "stage_templates[0].plugins[0]"},
		"template harness": {mutate(t, func(d map[string]any) { templates(d)[0].(map[string]any)["harness"] = "codex" }), "stage_templates[0].harness"},
		"template kind":    {mutate(t, func(d map[string]any) { templates(d)[0].(map[string]any)["kinds"] = []any{"human_review"} }), "stage_templates[0].kinds[0]"},
		"placeholder": {mutate(t, func(d map[string]any) {
			templates(d)[0].(map[string]any)["prompt"] = "Read {{plugin_root:foundry}}/x"
		}), "stage_templates[0].prompt"},
		"server of other plugin": {mutate(t, func(d map[string]any) {
			templates(d)[0].(map[string]any)["mcp_servers"] = []any{"foundry"}
		}), "stage_templates[0].mcp_servers"},
		"signal event": {mutate(t, func(d map[string]any) {
			templates(d)[1].(map[string]any)["signals"].([]any)[0].(map[string]any)["event"] = "verdict"
		}), "stage_templates[1].signals[0]"},
		"duplicate key": {[]byte(`{"id":"a","id":"b"}`), "$"},
		"two documents": {append(guildManifest(t), []byte("{}")...), "$"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(c.data)
			if err == nil || err.Path != c.path {
				t.Fatalf("got %v, want an error at %s", err, c.path)
			}
		})
	}
}

func TestValidateTreeRejectsMissingContent(t *testing.T) {
	m, perr := Parse(guildManifest(t))
	if perr != nil {
		t.Fatal(perr)
	}
	cases := map[string]struct {
		drop, replace string
		body          string
		path          string
	}{
		"not a plugin":        {drop: "plugins/forge/.claude-plugin/plugin.json", path: "plugins[0].path"},
		"missing hook script": {drop: "plugins/foundry/hooks/session-start-serena.sh", path: "hooks[0].command"},
		"hook not configured": {replace: "plugins/foundry/hooks/hooks.json", body: `{"hooks":{}}`, path: "hooks[0]"},
		"server not in plugin": {replace: "plugins/foundry/.claude-plugin/plugin.json", body: `{"name":"foundry"}`,
			path: "mcp_servers[0]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			files := maps.Clone(fixture.GuildFiles)
			delete(files, c.drop)
			if c.replace != "" {
				files[c.replace] = c.body
			}
			dir, commit := fixture.Repo(t, files)
			tree, err := OpenGitTree(t.Context(), dir, commit)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateTree(m, tree); err == nil || err.Path != c.path {
				t.Fatalf("got %v, want %s", err, c.path)
			}
		})
	}
}

func TestApprovalAndFreeze(t *testing.T) {
	data := guildManifest(t)
	m, perr := Parse(data)
	if perr != nil {
		t.Fatal(perr)
	}
	var required []string
	for _, p := range m.Permissions() {
		if !p.Optional {
			required = append(required, p.ID)
		}
	}
	if _, err := m.Approve(required[1:]); !errors.Is(err, ErrApproval) {
		t.Fatalf("missing required permission approved: %v", err)
	}
	if _, err := m.Approve(append(slices.Clone(required), "hook:unknown")); !errors.Is(err, ErrApproval) {
		t.Fatalf("undeclared permission approved: %v", err)
	}
	approved, err := m.Approve(required) // Optional hook and Serena declined.
	if err != nil {
		t.Fatal(err)
	}
	pin := Pin{ID: "guild", Version: "1.0.0", RepositoryURL: "https://github.com/alphabravo-oss/guild",
		Commit: strings.Repeat("a", 40), Manifest: data, Approved: approved}
	frozen, _, err := pin.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	if frozen.ManifestSHA256 != Digest(data) || frozen.Approves("hook:foundry-serena") || !frozen.Approves("mcp:foundry") {
		t.Fatalf("frozen %+v", frozen)
	}
	encoded, _ := json.Marshal(frozen)
	var back Frozen
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if _, err := back.Load(); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	back.Approved = append(back.Approved, "hook:foundry-serena")
	if _, err := back.Load(); err == nil {
		t.Fatal("widened approval loaded")
	}
	pin.Version = "1.0.1"
	if _, _, err := pin.Freeze(); err == nil {
		t.Fatal("version mismatch froze")
	}
	if id, version, template, ok := ParseTemplateRef("guild@1.0.0/foundry-build"); !ok || id != "guild" || version != "1.0.0" || template != "foundry-build" {
		t.Fatalf("template ref: %v %v %v %v", id, version, template, ok)
	}
	for _, bad := range []string{"guild/foundry-build", "guild@1.0/foundry-build", "Guild@1.0.0/x", "guild@1.0.0/"} {
		if _, _, _, ok := ParseTemplateRef(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	if got := ReplacePluginRoots("{{plugin_root:foundry}}/commands/start.md", map[string]string{"foundry": "/h/ext/foundry"}); got != "/h/ext/foundry/commands/start.md" {
		t.Fatal(got)
	}
}
