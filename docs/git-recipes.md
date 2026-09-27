# Factory-neutral recipes

Blaxsmith owns admission, authorization, immutable inputs, task ownership, AX dispatch, evidence and acceptance. AX remains the execution engine. A factory produces recipe content and chooses a workflow; it cannot grant itself access or redefine platform evidence.

Anvil is the default factory starter. Guild is an optional integration, imported explicitly. Both use the same `blaxsmith.recipe/v1alpha1` contract, freezer, scheduler, API and workers. There is no factory-specific compatibility path.

## Run a recipe

```sh
go run ./cmd/blaxsmith check --recipe examples/anvil/recipe.json --scope .
go run ./cmd/blaxsmith freeze --recipe examples/anvil/recipe.json --scope . > /tmp/anvil-bundle.json
# Optional Guild integration, explicitly declared by this recipe:
go run ./cmd/blaxsmith check --recipe examples/guild/recipe.json --scope examples/guild
```

The CLI reads committed files at `--ref` (HEAD by default), not working-tree changes. Native freezing needs Go and Git. The optional Guild Forge validator additionally needs Python. Example model selections must be adjusted to approved runtimes and account access before launch.

## Contract

A recipe supplies:

- `profiles`: explicit harness, model, effort, instruction and skill paths. Supported harnesses are Claude Code, Codex and OpenCode. OpenCode uses provider/model identifiers.
- `stages`: named tasks, prompt paths and acyclic dependencies. The factory chooses whether to include planning, interviews, reviews and checks. The engine does not impose an architect review sequence.
- `documents`: optional repository-relative context files, available to every stage. Use profile instructions for stage-specific context.
- `validation`: optional installed validator ID and named input paths. Unknown validators fail closed. No validator is inferred from a factory name or attempted as a fallback.
- `factory`: optional ID/version attribution. It grants no authority and does not select executable code.
- `required_checks`: names that must be required in the frozen project policy, or required extension gates. An empty list is valid.
- `acceptance`: explicitly `manual` or `policy`.
- `limits`: 0–10 correction cycles (0 permits one attempt per stage), idle timeout, and optional total runtime cap.

For example, the Guild integration declares:

```json
"validation": {
  "id": "guild-forge",
  "inputs": {
    "spec": "docs/spec.md",
    "transcript": "docs/interview.md"
  }
}
```

The platform freezes those files and invokes the validator installed by the application composition layer. The result records its ID, source revision, validator digest and report. The recipe/workflow/dispatch packages import neither Guild nor Anvil. Other factories can produce recipes without adding a validator. A new trusted validator is registered in the application, never executed from arbitrary repository code.

## User-selected quality

Project checks use `required`, `advisory` or `off`. Required failures block completion and acceptance; advisory failures are preserved as findings; off checks are not executed. An explicitly saved empty policy selects no automated checks. Recipe requirements cannot be downgraded by an advisory/off project check. Active project checks require a verify stage downstream of implementation.

Review stages can use `mode: "advisory"` or `mode: "required"` (the default). Omit a review stage to turn it off. Advisory reviews cannot own mandatory correction loops and cannot waive required extension evidence. Verification uses the platform runner and frozen argv, with no model invocation. Its runtime profile still supplies the approved sandbox configuration.

Every completed run gets an immutable evidence package. Manual acceptance awaits a human decision; policy acceptance records that the user-selected requirements were satisfied. Policy acceptance never fabricates a human approval and grants no merge/deploy authority. Empty/off checks are not claimed as passing tests. Required evidence is checked against the current attempt, candidate revision and frozen policy.

## Integration through the API

1. Configure the project source, approved runtime/model access and verification policy.
2. Commit prompts/documents/skills, or create an immutable recipe library version whose referenced files are committed.
3. Validate with RecipeService.ValidateRecipe. This validates structure; launch freezes actual files and performs integration validation.
4. Call WorkflowService.LaunchRun with project_id, launch_key, recipe_path **or** recipe_version_id, and scope. Document paths belong in the recipe, not launch arguments.
5. Read run/task state, events, interactions, immutable evidence and the current review package through the existing services. Package acceptance_mode distinguishes policy acceptance from a manual decision.

The browser and generated Go/TypeScript clients use these same operations. Existing browser authentication/CSRF and resource grants remain enforced. Project-scoped machine credentials and the MCP stdio bridge use the same stores; see [machine API and MCP](machine-api-and-mcp.md). Worker bx tools remain attempt-scoped.

## Frozen inputs and safety boundaries

The bundle records the source commit, recipe path, scope, parsed recipe, deterministic stage order, optional validator result and sorted artifacts with exact bytes and SHA-256 digests. Documents, validator inputs, prompts, declared instructions/skills and applicable AGENTS.md files are frozen. Mutating a selected profile, factory attribution, policy or document changes provenance. Editing a library recipe creates a new version; active runs do not change.

The compiler rejects unknown/duplicate JSON fields, invalid dependency graphs, path traversal, symlink artifacts, unresolved LFS pointers and submodules in scope. Limits are 64 stages, 256 files, 1 MiB per artifact and 16 MiB total. A digest identifies bytes; it is not authorization or proof that code passed checks.

Anvil is seeded for new organizations and granted to members, admins and owners. Seed replay never restores a revoked grant. The migration adds Anvil to existing organizations without rewriting their saved versions or grants. Guild is not seeded for newly created organizations.

## Discovery and optional launch preview

`WorkflowService.GetPlatformCapabilities` reports the recipe schema, supported harnesses, acceptance/check modes, and stage limits. This describes implemented platform support, not a caller's model grants or current runtime readiness.

`WorkflowService.PreviewRun` accepts `project_id`, exactly one of `recipe_path` or `recipe_version_id`, and `scope`. It fetches the configured Git source, resolves the same recipe and extension grants as launch, and returns the source commit, resolved recipe, ordered stages, frozen input paths/digests, selected checks, and readiness blockers. It creates no run and starts no worker. Browser sessions and CSRF are required because preview uses private source credentials and checks runtime readiness.

The launch screen renders selectable stage cards with execution details, acceptance, checks and frozen inputs. Preview is optional. If used, `LaunchRun.expected_bundle_sha256` and `expected_verification_sha256` must be supplied together. A changed source, recipe, scope, input artifact, or check policy rejects admission (`aborted` for mismatched digests). Invalid policies still fail validation. Refresh the preview or explicitly dismiss it to launch without these pins. Admission always rechecks live authority, project settings and installed extensions; a preview is never an approval token.

Readiness is a snapshot. A disconnected dispatcher or missing model/worker/egress setup prevents readiness, but the resolved recipe can still be inspected. Invalid recipe/source inputs return a request error. These endpoints also accept scoped credentials on the separate machine API surface; browser sessions retain CSRF enforcement.

## Current execution limits

The run branch transport supports one implement stage per run; unsupported multi-writer graphs are rejected explicitly. Other stages may form a DAG, including parallel reviews. A future multi-branch integration capability must define candidate composition and fresh verification before this restriction is lifted. Human request-changes currently targets the implement stage, so review-only runs can be approved but cannot request an implementation repair. Extension execution remains limited to the capabilities documented in extensions-and-runtimes.md.

This change does not migrate the AX runtime pin or claim live AX qualification. The Anvil starter is an executable starting workflow; the full interview, planning, model-routing and long-goal factory remains the separate Anvil implementation plan.

The native [goal workspace](goal-workspace.md) now owns briefs, context messages, and starter-question decisions before a run exists. Saved context and versioned plans compile into immutable task packets through [native execution](anvil-execution.md). [Scoped project inputs](project-inputs.md) add shared stack profiles, rules, and agent definitions for any factory.

A profile may include `"connection": "<exact model connection ID>"` to pin its billing account. The initiating principal must hold the applicable personal grant or the project must select that workload grant. Revocation, ownership change or a missing matching grant stops dispatch; the platform never falls back to a different account for a pinned profile. Omit this field to use the documented personal-first/project grant selection. Account pinning grants no new authority.
