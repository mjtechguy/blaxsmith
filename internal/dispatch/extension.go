package dispatch

import (
	"fmt"
	"slices"

	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
)

// extensionMount derives an embedded stage's approved extension slice from
// the frozen bundle, and returns the template prompt to prepend. It returns
// nil for a stage without a template. Only permissions the administrator
// approved at install reach the worker; anything else is left out.
func extensionMount(bundle *recipe.Bundle, stage recipe.Stage) (*tooladapter.ExtensionMount, string, error) {
	if stage.Template == "" {
		return nil, "", nil
	}
	id, version, name, ok := extension.ParseTemplateRef(stage.Template)
	if !ok || bundle == nil {
		return nil, "", tooladapter.ErrBlocked
	}
	index := slices.IndexFunc(bundle.Extensions, func(f extension.Frozen) bool { return f.ID == id && f.Version == version })
	if index < 0 {
		return nil, "", fmt.Errorf("%w: stage template %s is not frozen in the bundle", tooladapter.ErrBlocked, stage.Template)
	}
	frozen := bundle.Extensions[index]
	m, err := frozen.Load()
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", tooladapter.ErrBlocked, err)
	}
	t, ok := m.Template(name)
	if !ok {
		return nil, "", tooladapter.ErrBlocked
	}
	mount := &tooladapter.ExtensionMount{ID: frozen.ID, Version: frozen.Version, RepositoryURL: frozen.RepositoryURL,
		Commit: frozen.Commit, ManifestSHA256: frozen.ManifestSHA256, Template: name, Signals: t.Signals}
	for _, pluginID := range t.Plugins {
		plugin := tooladapter.PluginMount{ID: pluginID, Path: m.PluginPath(pluginID)}
		for _, serverID := range t.MCPServers {
			for _, s := range m.MCPServers {
				if s.ID == serverID && s.Plugin == pluginID && frozen.Approves("mcp:"+s.ID) {
					plugin.MCPServers = append(plugin.MCPServers, s.Server)
				}
			}
		}
		for _, hookID := range t.Hooks {
			for _, h := range m.Hooks {
				if h.ID == hookID && h.Plugin == pluginID && frozen.Approves("hook:"+h.ID) {
					plugin.Hooks = append(plugin.Hooks, tooladapter.PluginHook{Event: h.Event, Command: h.Command})
				}
			}
		}
		mount.Plugins = append(mount.Plugins, plugin)
	}
	for _, serverID := range t.MCPServers {
		for _, s := range m.MCPServers {
			if s.ID == serverID && s.Plugin == "" && frozen.Approves("mcp:"+s.ID) {
				mount.LocalMCP = append(mount.LocalMCP, tooladapter.LocalMCPServer{Name: s.ID, URL: s.URL})
			}
		}
	}
	if t.Mode != "embedded" || (m.NativeSubagents && !frozen.Approves("subagents")) {
		return nil, "", fmt.Errorf("%w: template %s is not runnable with its approved permissions", tooladapter.ErrBlocked, stage.Template)
	}
	header := fmt.Sprintf("Extension: %s@%s template %s (commit %s, manifest sha256 %s)\n\n", frozen.ID, frozen.Version, name,
		frozen.Commit, frozen.ManifestSHA256)
	return mount, header + t.Prompt, nil
}
