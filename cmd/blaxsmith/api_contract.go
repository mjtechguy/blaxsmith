package main

import (
	"encoding/json"
	"errors"
	"io"

	api "github.com/mjtechguy/blaxsmith/gen/go/blaxsmith/api/v1"
)

// This offline reference uses the same projection and schemas as MCP. Live
// discovery still decides which operations a specific credential may invoke.
func writeAPIContract(out io.Writer) error {
	type operation struct {
		Method        string         `json:"method"`
		Path          string         `json:"path"`
		MCPTool       string         `json:"mcp_tool"`
		Scope         string         `json:"scope"`
		Resource      string         `json:"resource,omitempty"`
		ResourceField string         `json:"resource_field,omitempty"`
		Description   string         `json:"description"`
		Input         map[string]any `json:"input"`
		Output        map[string]any `json:"output"`
	}
	contract := struct {
		Schema       string      `json:"schema_version"`
		Instructions string      `json:"instructions"`
		Operations   []operation `json:"operations"`
	}{Schema: "blaxsmith.machine-contract/v1", Instructions: agentInstructions}
	service := api.File_blaxsmith_api_v1_workflow_proto.Services().ByName("WorkflowService")
	for i := 0; i < service.Methods().Len(); i++ {
		method := service.Methods().Get(i)
		name := string(method.Name())
		access, ok := machineOperations[name]
		if !ok {
			continue
		}
		description := mcpDescriptions[name]
		if description == "" {
			return errors.New("missing machine operation description: " + name)
		}
		contract.Operations = append(contract.Operations, operation{Method: name, Path: "/machine/blaxsmith.api.v1.WorkflowService/" + name, MCPTool: "blaxsmith_" + name, Scope: access.scope, Resource: access.resource, ResourceField: access.field, Description: description, Input: protoInputSchema(method.Input()), Output: protoInputSchema(method.Output())})
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(contract)
}
