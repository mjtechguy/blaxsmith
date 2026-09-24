package extension

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/mjtechguy/blaxsmith/internal/limit"
)

// Tree is a read-only view of the repository at the pinned commit.
type Tree interface {
	// IsFile reports a regular (non-symlink, non-submodule) file.
	IsFile(name string) bool
	// IsDir reports a directory containing at least one regular file.
	IsDir(name string) bool
	Read(name string) ([]byte, error)
}

// ValidateTree checks that every path the manifest references exists at the
// commit, that imported plugins are Claude Code plugins, and that declared
// hooks and plugin MCP servers exist in those plugins. It reads JSON only.
func ValidateTree(m Manifest, tree Tree) *Error {
	pluginJSON := map[string]map[string]json.RawMessage{}
	for i, p := range m.Plugins {
		at := fmt.Sprintf("plugins[%d].path", i)
		manifest := p.Path + "/.claude-plugin/plugin.json"
		if !tree.IsDir(p.Path) || !tree.IsFile(manifest) {
			return fail(at, "%q is not a Claude Code plugin directory (.claude-plugin/plugin.json) at this commit", p.Path)
		}
		data, err := tree.Read(manifest)
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(data, &fields) != nil {
			return fail(at, "%s is not a JSON object", manifest)
		}
		pluginJSON[p.ID] = fields
	}
	files := []struct {
		at, name string
	}{}
	for i, s := range m.Skills {
		files = append(files, struct{ at, name string }{fmt.Sprintf("skills[%d].path", i), s.Path})
	}
	for i, r := range m.Rules {
		files = append(files, struct{ at, name string }{fmt.Sprintf("rules[%d].path", i), r.Path})
	}
	for i, a := range m.Agents {
		files = append(files, struct{ at, name string }{fmt.Sprintf("agents[%d].path", i), a.Path})
	}
	for i, r := range m.Recipes {
		files = append(files, struct{ at, name string }{fmt.Sprintf("recipes[%d].path", i), r.Path})
	}
	for i, t := range m.StageTemplates {
		for j, s := range t.Skills {
			files = append(files, struct{ at, name string }{fmt.Sprintf("stage_templates[%d].skills[%d]", i, j), s})
		}
	}
	for _, f := range files {
		if !tree.IsFile(f.name) {
			return fail(f.at, "%q is not a regular file at this commit", f.name)
		}
	}
	for i, s := range m.MCPServers {
		if s.Plugin == "" {
			continue
		}
		var servers map[string]json.RawMessage
		if raw, ok := pluginJSON[s.Plugin]["mcpServers"]; !ok || json.Unmarshal(raw, &servers) != nil || servers[s.Server] == nil {
			return fail(fmt.Sprintf("mcp_servers[%d]", i), "plugin %q declares no MCP server %q in plugin.json", s.Plugin, s.Server)
		}
	}
	for i, h := range m.Hooks {
		at := fmt.Sprintf("hooks[%d]", i)
		root := m.PluginPath(h.Plugin)
		if !tree.IsFile(path.Join(root, h.Command)) {
			return fail(at+".command", "%q is not a regular file in plugin %q", h.Command, h.Plugin)
		}
		data, err := tree.Read(path.Join(root, "hooks/hooks.json"))
		var config struct {
			Hooks map[string][]struct {
				Hooks []struct{ Type, Command string }
			}
		}
		if err != nil || json.Unmarshal(data, &config) != nil {
			return fail(at, "plugin %q has no readable hooks/hooks.json", h.Plugin)
		}
		found := false
		for _, group := range config.Hooks[h.Event] {
			for _, entry := range group.Hooks {
				found = found || entry.Type == "command" && entry.Command == "${CLAUDE_PLUGIN_ROOT}/"+h.Command
			}
		}
		if !found {
			return fail(at, "plugin %q hooks/hooks.json has no %s command ${CLAUDE_PLUGIN_ROOT}/%s", h.Plugin, h.Event, h.Command)
		}
	}
	return nil
}

// GitTree reads a local repository at one commit without filters, hooks,
// lazy fetches, or replacement objects.
type GitTree struct {
	repo, commit string
	files        map[string]string // path -> blob id, regular files only
}

// OpenGitTree lists the regular files of commit in repo.
func OpenGitTree(ctx context.Context, repo, commit string) (*GitTree, error) {
	t := &GitTree{repo: repo, commit: commit, files: map[string]string{}}
	out, err := t.git(ctx, "ls-tree", "--full-tree", "-r", "-z", commit)
	if err != nil {
		return nil, err
	}
	for record := range strings.SplitSeq(string(out), "\x00") {
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			continue
		}
		if fields[0] == "100644" || fields[0] == "100755" {
			t.files[name] = fields[2]
		}
	}
	return t, nil
}

func (t *GitTree) IsFile(name string) bool {
	_, ok := t.files[name]
	return ValidPath(name) && ok
}

func (t *GitTree) IsDir(name string) bool {
	if !ValidPath(name) {
		return false
	}
	for file := range t.files {
		if strings.HasPrefix(file, name+"/") {
			return true
		}
	}
	return false
}

// Files lists regular files under dir ("" for all).
func (t *GitTree) Files(dir string) []string {
	var out []string
	for file := range t.files {
		if dir == "" || strings.HasPrefix(file, dir+"/") {
			out = append(out, file)
		}
	}
	return out
}

func (t *GitTree) Read(name string) ([]byte, error) {
	blob, ok := t.files[name]
	if !ValidPath(name) || !ok {
		return nil, fmt.Errorf("%q is not a regular file at %s", name, t.commit)
	}
	out, err := t.git(context.Background(), "cat-file", "-s", blob)
	if err != nil {
		return nil, err
	}
	if size, err := strconv.Atoi(strings.TrimSpace(string(out))); err != nil || size > MaxManifestBytes*16 {
		return nil, fmt.Errorf("%q is too large", name)
	}
	return t.git(context.Background(), "cat-file", "blob", blob)
}

func (t *GitTree) git(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"--no-replace-objects", "--literal-pathspecs", "-C", t.repo}, args...)...) // #nosec G204 -- fixed executable, separate argv
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
	stdout, stderr := limit.Buffer{Max: 4 << 20}, limit.Buffer{Max: 8 << 10}
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}
