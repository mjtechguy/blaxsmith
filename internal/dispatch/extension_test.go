package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/extension"
	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

func guildFrozen(t *testing.T, optional ...string) extension.Frozen {
	t.Helper()
	data, err := os.ReadFile("../../examples/extensions/guild/blaxsmith-extension.json")
	if err != nil {
		t.Fatal(err)
	}
	m, perr := extension.Parse(data)
	if perr != nil {
		t.Fatal(perr)
	}
	approved := optional
	for _, p := range m.Permissions() {
		if !p.Optional {
			approved = append(approved, p.ID)
		}
	}
	frozen, _, err := extension.Pin{ID: "guild", Version: "1.0.0", RepositoryURL: "https://github.com/example/guild",
		Commit: strings.Repeat("b", 40), Manifest: data, Approved: approved}.Freeze()
	if err != nil {
		t.Fatal(err)
	}
	return frozen
}

func TestExtensionMountHonoursApprovals(t *testing.T) {
	stage := recipe.Stage{ID: "build", Kind: "implement", Prompt: "build.md", Template: "guild@1.0.0/foundry-build"}
	bundle := &recipe.Bundle{Recipe: recipe.Recipe{Documents: []string{"spec.md", "t.md"}}, Extensions: []extension.Frozen{guildFrozen(t)}}
	mount, prompt, err := extensionMount(bundle, stage)
	if err != nil || mount == nil || mount.Commit != strings.Repeat("b", 40) || len(mount.Plugins) != 1 {
		t.Fatalf("mount: %+v, %v", mount, err)
	}
	if len(mount.Plugins[0].MCPServers) != 1 || len(mount.Plugins[0].Hooks) != 0 || len(mount.LocalMCP) != 0 {
		t.Fatalf("unapproved optional permissions reached the worker: %+v", mount)
	}
	if !strings.Contains(prompt, "guild@1.0.0 template foundry-build") || !strings.Contains(prompt, mount.ManifestSHA256) {
		t.Fatalf("template prompt: %q", prompt)
	}

	bundle.Extensions = []extension.Frozen{guildFrozen(t, "mcp:serena", "hook:foundry-serena")}
	mount, _, err = extensionMount(bundle, stage)
	if err != nil || len(mount.Plugins[0].Hooks) != 1 || len(mount.LocalMCP) != 1 {
		t.Fatalf("approved optional permissions missing: %+v, %v", mount, err)
	}

	if m, p, err := extensionMount(bundle, recipe.Stage{ID: "plain", Kind: "plan"}); m != nil || p != "" || err != nil {
		t.Fatalf("plain stage: %v %q %v", m, p, err)
	}
	for _, ref := range []string{"guild@2.0.0/foundry-build", "guild@1.0.0/missing", "not a ref"} {
		if _, _, err := extensionMount(bundle, recipe.Stage{ID: "x", Kind: "implement", Template: ref}); !errors.Is(err, tooladapter.ErrBlocked) {
			t.Fatalf("%s: %v", ref, err)
		}
	}
	tampered := guildFrozen(t)
	tampered.Approved = append(tampered.Approved, "hook:foundry-serena")
	bundle.Extensions = []extension.Frozen{tampered}
	if _, _, err := extensionMount(bundle, stage); !errors.Is(err, tooladapter.ErrBlocked) {
		t.Fatalf("approval list edited after freeze accepted: %v", err)
	}
}

func TestFrozenPromptAppendsTemplateBeforeHandoff(t *testing.T) {
	artifact := func(name, body string) recipe.Artifact {
		sum := sha256.Sum256([]byte(body))
		return recipe.Artifact{Path: name, Data: []byte(body), SHA256: hex.EncodeToString(sum[:])}
	}
	bundle := &recipe.Bundle{Source: recipe.Source{Commit: strings.Repeat("a", 40), Scope: "src"},
		Artifacts:  []recipe.Artifact{artifact("spec.md", "requirements"), artifact("t.md", "decisions"), artifact("build.md", "stage prompt")},
		Extensions: []extension.Frozen{guildFrozen(t)}}
	task := workflow.FrozenTask{Bundle: bundle,
		Stage: recipe.Stage{ID: "build", Kind: "implement", Prompt: "build.md", Template: "guild@1.0.0/foundry-build"}}
	prompt, _, err := frozenPrompt(task, "upstream", task.Bundle.Source.Commit)
	if err != nil {
		t.Fatal(err)
	}
	stageAt := strings.Index(prompt, "stage prompt")
	templateAt := strings.Index(prompt, "Extension stage template")
	handoffAt := strings.Index(prompt, "untrusted context")
	if stageAt < 0 || templateAt < stageAt || handoffAt < templateAt {
		t.Fatalf("prompt order: %q", prompt)
	}
}
