# Scoped rules, stacks and agent definitions

Use a committed `blaxsmith.inputs/v1alpha1` JSON file to share project guidance. Any factory can select it with a recipe profile's `inputs` path and optional `agent` ID. Anvil exposes the same fields under Repository rules and skills for planning, implementation, and independent review. The recipe editor also exposes them. There is no Guild dependency.

```json
{
  "schema_version": "blaxsmith.inputs/v1alpha1",
  "rules": [
    {"id": "api", "scope": "internal", "file": "docs/api-rules.md", "settings": {"errors": "typed"}},
    {"id": "web", "scope": "frontend", "file": "docs/web-rules.md", "settings": {"errors": "visible"}}
  ],
  "stack": {
    "name": "Go and React",
    "prerequisites": ["Go matching go.mod", "Node matching frontend/package.json", "isolated PostgreSQL for database tests"],
    "conventions": ["Reuse existing stores and tenant/session guards"],
    "commands": [{"id": "go-test", "command": ["go", "test", "./..."]}],
    "examples": ["docs/git-recipes.md"]
  },
  "agents": {
    "implementer": {
      "responsibility": "Implement a bounded assignment while preserving existing behavior",
      "instructions": ["docs/engineering.md"],
      "harnesses": ["codex", "claude-code", "opencode"],
      "capabilities": ["read_repository", "ask_user", "publish_artifacts", "write_candidate"],
      "input_expectations": ["Pinned source, task packet, predecessor evidence"],
      "output_expectations": ["Candidate revision and evidence references"],
      "completion_criteria": ["Meet task acceptance and report unresolved uncertainty"]
    }
  }
}
```

Paths in this example must exist in the selected Git commit. A profile selects it as:

```json
{"harness":"codex","model":"your-approved-model","effort":"medium","inputs":".blaxsmith/inputs.json","agent":"implementer"}
```

The API fields are `inputs_file` and `agent_definition` on StartGoalPlanningRequest, GoalExecutionOptions, and GoalReviewOptions. Machine API and MCP use these same fields. An empty selection leaves existing input behavior unchanged.

## Resolution and authority

A rule covers its directory and descendants. The freezer includes rules whose scope intersects the selected repository scope. A sibling rule outside that scope is excluded, including its bytes. Root-scope runs include descendant rules with explicit directory labels in worker prompts. Applicable AGENTS.md files now carry directory labels too. These are guidance scopes; they do not sandbox write paths.

For overlapping scopes, two structured settings with the same key and different values reject the preview/launch. The diagnostic names both rule IDs and files. Separate sibling scopes may have different values. No silent prompt-order override is used. Prose cannot be deterministically reconciled: the preview flags that review is needed and workers are instructed to surface unresolved contradictions. No automatic prose conflict detection is claimed.

Agent instructions and skills extend the explicit profile selections, deduplicated without reordering. The selected harness/model/effort remains explicit. Optional `harnesses` and `models` arrays restrict supported selections; they never pick a model silently. Skills still pass the shared adapter validation, including frontmatter and support-file restrictions. The verifier uses the platform check runner and does not inherit an implementation agent definition.

Capabilities describe required platform behavior: `read_repository`, `ask_user`, `publish_artifacts`, and `write_candidate`. Unsupported names reject input loading; `write_candidate` is allowed only on an implementation stage. They confer no credential, filesystem, network, tool, approval, spending, or delivery grant. All existing runtime and authority checks remain mandatory. A completion criterion is an instruction, not proof that the work succeeded.

## Stack commands and environment

Stack commands use the existing project command contract: IDs and bounded argv arrays, no execution while loading. Adopt desired commands into project verification settings and select their required/advisory/off modes. Prerequisites remain visible requirements for environment setup, not assertions that the environment already satisfies them.

When root `package.json` exists, the freezer preserves it in the bundle and reports its package-manager pin and engine constraints. A requested `package_manager` must agree with the detected manager (including lockfiles) and any exact packageManager pin. It never changes a lockfile or installs packages. Nested workspaces and non-Node prerequisite satisfaction still require project inspection and selected checks; this contract does not invent a general environment solver.

## Provenance, import and export

The frozen bundle stores selected effective inputs by profile, the input file SHA-256, source commit, scope, rules, stack, and selected agent. Original configuration and selected referenced files retain exact bytes and hashes. Worker prompts receive only selected definitions/rules; unselected agent text is not injected. Configuration is strictly decoded: unknown/duplicate fields, unsupported schema versions, unsafe paths, missing files, and symlinks reject loading. Limits: 64 KiB configuration, 64 rules, 32 agent definitions, plus existing artifact/bundle/request bounds.

Launch preview displays effective inputs, scope, conflicts, diagnostics, and exact resolved JSON. Export selected configuration downloads a valid inputs document containing the selected rule/stack/agent subset. Referenced files remain repository paths and must travel with the configuration; this is not an executable pack. The source provenance is visible separately in the preview and retained in the frozen bundle. Editing Git files creates new provenance on the next preview; an admitted run remains immutable.

Local qualification extends the Git/PostgreSQL/correction E2E: scoped sibling exclusion, conflicting settings, unsupported capability/model, stack mismatch, unsafe paths, resolved worker request construction, unselected-agent exclusion, and immutable replay after checkout edits. Browser E2E covers editable input selection. Actual model compliance with prose guidance remains part of live AX qualification.
