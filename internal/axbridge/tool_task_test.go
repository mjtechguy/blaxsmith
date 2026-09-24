package axbridge

import (
	"reflect"
	"strings"
	"testing"

	"github.com/mjtechguy/blaxsmith/internal/recipe"
	"github.com/mjtechguy/blaxsmith/internal/tooladapter"
	"github.com/mjtechguy/blaxsmith/internal/workflow"
	"gopkg.in/yaml.v3"
)

func TestToolTaskPinsPublicCommandWithoutCredential(t *testing.T) {
	image := "example/runner@sha256:" + strings.Repeat("a", 64)
	attempt := workflow.Attempt{ID: "attempt-1", OrganizationID: "org-1"}
	request := tooladapter.Request{AttemptID: attempt.ID, RepositoryURL: "https://github.com/owner/repo",
		SourceCommit: strings.Repeat("a", 40), Runtime: tooladapter.Runtime{Harness: "opencode", Image: image,
			Binary: "/opt/blaxsmith/bin/opencode", BinarySHA256: strings.Repeat("b", 64), Version: "2.0.14",
			Supported: []tooladapter.ModelEffort{{Model: "openai/gpt-6-luna", Effort: "high"}}},
		Profile: recipe.Profile{Harness: "opencode", Model: "openai/gpt-6-luna", Effort: "high"},
		Prompt:  "Implement the task", TimeoutSeconds: 60, MaxOutputBytes: 1024}
	bridge := &Bridge{Workflow: &workflow.Store{}, AX: &fakeAX{}, Actor: fakeActor{}, Image: image,
		Pool: "pool", Signer: "signer", Storage: "gs://snapshots/", Tool: &request,
		Workspace: "source", Gateway: "public-egress"}
	task, err := bridge.task(attempt)
	if err != nil {
		t.Fatal(err)
	}
	command, err := tooladapter.Command(request)
	_, hasDebug := task.Spec["debug"]
	if err != nil || !reflect.DeepEqual(task.Spec["command"], []any{command[0], command[1]}) ||
		!reflect.DeepEqual(task.Spec["workspaces"], []any{map[string]any{"name": "source", "path": "/workspace"}}) ||
		!reflect.DeepEqual(task.Spec["gateway"], map[string]any{"name": "public-egress"}) ||
		!reflect.DeepEqual(task.Spec["resources"], map[string]any{
			"requests": map[string]any{"cpu": "1", "memory": "1Gi"},
			"limits":   map[string]any{"cpu": "1", "memory": "1Gi"},
		}) ||
		hasDebug ||
		strings.Contains(command[1], "api_key") || strings.Contains(command[1], "credential") {
		t.Fatalf("task contains unexpected command: %+v: %v", task, err)
	}
	encoded, err := yaml.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	var readback Task
	if err := yaml.Unmarshal(encoded, &readback); err != nil || !sameTask(readback, task) {
		t.Fatalf("AX YAML readback differs from the frozen task: %v", err)
	}
	request.Runtime.Image = "example/other@sha256:" + strings.Repeat("a", 64)
	if _, err := bridge.task(attempt); err == nil {
		t.Fatal("mismatched runner image was accepted")
	}
	request.Runtime.Image = image
	bridge.Gateway = ""
	if _, err := bridge.task(attempt); err == nil {
		t.Fatal("tool task without AX gateway was accepted")
	}
}
