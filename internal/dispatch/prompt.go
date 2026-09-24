package dispatch

import (
	"bytes"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
)

const maxPromptBytes = 1 << 20

// frozenPrompt uses only bytes in the run's immutable bundle. Tool profiles
// with separately declared skills/instructions are blocked by tooladapter.
func frozenPrompt(task workflow.FrozenTask) (string, error) {
	if task.Bundle == nil || task.Stage.Prompt == "" || task.Bundle.Source.Spec == "" || task.Bundle.Source.Transcript == "" {
		return "", tooladapter.ErrBlocked
	}
	artifacts := make(map[string][]byte, len(task.Bundle.Artifacts))
	agents := []string{}
	for _, artifact := range task.Bundle.Artifacts {
		if _, exists := artifacts[artifact.Path]; exists {
			return "", tooladapter.ErrBlocked
		}
		artifacts[artifact.Path] = artifact.Data
		if path.Base(artifact.Path) == "AGENTS.md" {
			agents = append(agents, artifact.Path)
		}
	}
	slices.Sort(agents)
	paths := append(agents, task.Bundle.Source.Spec, task.Bundle.Source.Transcript, task.Stage.Prompt)
	var prompt bytes.Buffer
	fmt.Fprintf(&prompt, "Frozen source commit: %s\nScope: %s\nStage: %s (%s)\n",
		task.Bundle.Source.Commit, task.Bundle.Source.Scope, task.Stage.ID, task.Stage.Kind)
	for _, name := range paths {
		data, ok := artifacts[name]
		if !ok || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 || strings.ContainsAny(name, "\r\n\x00") {
			return "", tooladapter.ErrBlocked
		}
		fmt.Fprintf(&prompt, "\n--- %s ---\n", name)
		prompt.Write(data)
		if prompt.Len() > maxPromptBytes {
			return "", tooladapter.ErrBlocked
		}
	}
	return prompt.String(), nil
}
