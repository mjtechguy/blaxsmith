package tooladapter

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// A Claude setup-token (`claude setup-token`) is the owner's own long-lived
// subscription credential. It arrives through the same leased API-key path as
// ANTHROPIC_API_KEY; the prefix is what tells the two apart. The platform only
// grants it to runs started by its owner (access.ClaudeSetupTokenAuth).
const claudeSetupTokenPrefix = "sk-ant-oat"

// redactOnlyEnvNames are hidden by the redactors but never accepted in the
// incoming credential environment: the setup-token reaches CLAUDE_CODE_OAUTH_TOKEN
// only through claudeSubscriptionMode.
var redactOnlyEnvNames = []string{"CLAUDE_CODE_OAUTH_TOKEN"}

// ancestorClaudeConfig lists what `--setting-sources project` would pick up
// from the workdir's parents. The probe (docs/claude-subscription-probe.json)
// showed ancestor project skills load in non-bare mode, so any of these above
// the checkout makes subscription mode refuse to run.
var ancestorClaudeConfig = []string{".claude", "CLAUDE.md", "CLAUDE.local.md", ".mcp.json"}

// claudeSubscriptionMode rewrites a Claude Code launch that carries a
// setup-token. `--bare` ignores CLAUDE_CODE_OAUTH_TOKEN, so the token needs
// non-bare mode; the isolation `--bare` gave is rebuilt with the flags and
// environment the credential-free probe verified. API-key launches and every
// other harness pass through unchanged.
func claudeSubscriptionMode(harness, workdir, home string, args, resume, env []string) ([]string, []string, []string, error) {
	if harness != "claude-code" {
		return args, resume, env, nil
	}
	token, index := "", -1
	for i, entry := range env {
		if value, ok := strings.CutPrefix(entry, "ANTHROPIC_API_KEY="); ok && strings.HasPrefix(value, claudeSetupTokenPrefix) {
			token, index = value, i
		}
	}
	if index < 0 {
		return args, resume, env, nil
	}
	if err := refuseAncestorClaudeConfig(workdir); err != nil {
		return nil, nil, nil, err
	}
	out := append(append([]string(nil), env[:index]...), env[index+1:]...)
	out = append(out, "CLAUDE_CODE_OAUTH_TOKEN="+token,
		"CLAUDE_CONFIG_DIR="+filepath.Join(home, ".claude"),
		"CLAUDE_CODE_DISABLE_CLAUDE_MDS=1", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	rewritten, err := unbare(args)
	if err != nil {
		return nil, nil, nil, err
	}
	resumed, err := unbare(resume)
	if err != nil {
		return nil, nil, nil, err
	}
	return rewritten, resumed, out, nil
}

// unbare replaces the leading `--bare` with the probe-verified isolation flags
// and adds disableAllHooks to the existing --settings JSON object.
func unbare(args []string) ([]string, error) {
	if len(args) == 0 || args[0] != "--bare" {
		return nil, fmt.Errorf("%w: Claude launch is not in bare form", ErrBlocked)
	}
	out := []string{"--setting-sources", "project", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`}
	hooksOff := false
	for i := 1; i < len(args); i++ {
		if args[i] == "--settings" && i+1 < len(args) && strings.HasSuffix(args[i+1], "}") {
			out = append(out, args[i], strings.TrimSuffix(args[i+1], "}")+`,"disableAllHooks":true}`)
			hooksOff = true
			i++
			continue
		}
		out = append(out, args[i])
	}
	if !hooksOff {
		return nil, fmt.Errorf("%w: Claude launch has no settings to disable hooks", ErrBlocked)
	}
	return out, nil
}

func refuseAncestorClaudeConfig(workdir string) error {
	for dir := filepath.Dir(filepath.Clean(workdir)); ; dir = filepath.Dir(dir) {
		for _, name := range ancestorClaudeConfig {
			if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
				return fmt.Errorf("%w: %s above the checkout would load in Claude subscription mode", ErrBlocked, filepath.Join(dir, name))
			} else if !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("%w: cannot inspect %s: %v", ErrBlocked, dir, err)
			}
		}
		if dir == filepath.Dir(dir) {
			return nil
		}
	}
}
