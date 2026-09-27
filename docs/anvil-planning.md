# Anvil planning runs and plan versions

Open **Project → Goals → goal → Plan the work**. Expand **Start an Anvil planning run**, select a connected model and its effort, and choose a scope and time limit. The existing model picker lists account models; the platform rechecks actual project grants and runtime compatibility. This does not rank models or promise a cost estimate.

The Anvil recipe has a `plan` stage followed by `verify`. Planning uses the selected model through the ordinary dispatcher and AX runtime. Verification executes the project's frozen checks through the existing platform verifier, without a second model conversation. The recipe allows no correction retries and caps each stage at 60–3600 seconds. These are time bounds, **not token or spending caps**. Project checks retain their required/advisory/off modes. The run uses manual acceptance and contains no implementation stage or code-push operation.

The planner can ask durable `bx ask` questions. Answer them directly in the goal workspace or open the linked run for its existing pause, halt, steering, and evidence controls. Adding a goal message does not steer an already running planner: it changes the context revision for a future run.

## Frozen inputs and boundaries

Anvil captures the original brief, current starter answers, and every saved context message at one goal revision. It refuses stale revisions and context over 256 KiB rather than silently truncating. Goal context and native prompts are bounded `recipe.Input.PlatformFiles` under `.blaxsmith/platform/`, with explicit source provenance. They are digest-bound in the immutable bundle and delivered inline to the worker. They are not represented as committed checkout files. Existing Git instruction files and applicable `AGENTS.md` remain pinned and checked against the checkout. Git collisions with platform inputs are rejected.

Application composition chooses Anvil and validates its artifact schema. The workflow, recipe, dispatch, and goal-plan persistence layers remain factory-neutral. Guild is not involved. The native browser entry point does not yet expose third-party factory initialization.

Launching requires a configured, nonempty committed Git source, saved project verification policy (which may contain no checks), a connected dispatcher, approved runtime, and authorized model access. Truly empty repositories need the separate greenfield bootstrap flow. The goal/run revision association is written atomically with frozen run admission, after checking the current goal revision and live caller session.

Before admission, shared preflight assembles each stage's initial worker request and validates selected native skills. Invalid skill frontmatter and requests above the 120 KiB JSON-encoded command limit return a stage-specific error without creating a run. This worker limit can reject inputs below the compilation ceilings, especially text that expands during JSON escaping. Reduce instructions or split the work; later handoffs and runtime bindings are checked again during dispatch.

## Structured artifact

The planner writes `anvil-plan.json` and registers it with:

```sh
bx artifact --json '{"id":"anvil-plan","path":"anvil-plan.json","kind":"report","title":"Anvil implementation plan","renderer":"json"}'
```

The schema is `anvil.plan/v1alpha1`. A small valid plan can be:

```json
{
  "schema_version": "anvil.plan/v1alpha1",
  "title": "Save a search",
  "summary": "Extend the existing search service and verify persistence.",
  "assumptions": [],
  "out_of_scope": ["Sharing saved searches"],
  "open_questions": [],
  "requirements": [{
    "id": "R1",
    "description": "Members can restore a saved search after reload.",
    "sources": ["brief"],
    "examples": ["Save a query, reload, and restore the same filters."]
  }],
  "phases": [{"id": "P1", "title": "Deliver saved searches", "outcome": "Persistence works through the UI."}],
  "tasks": [{
    "id": "T1", "title": "Persist and restore searches", "phase": "P1",
    "reason": "Keep the user journey in the existing service.",
    "depends_on": [], "requirement_ids": ["R1"],
    "instructions": "Inspect the search service and callers. Extend its existing persistence and error handling. Preserve previous settings when validation fails.",
    "acceptance": ["Reload restores filters", "Invalid input preserves saved settings"],
    "validation": ["Focused service tests and a browser reload journey"]
  }]
}
```

Validation rejects unknown fields, dangling references, cycles, duplicate IDs, unused phases, uncovered requirements, missing instructions/acceptance criteria, and unknown context citations. Limits: 256 KiB, 128 requirements, 16 phases, 64 tasks. Sources can be `brief`, `question:<answered starter ID>`, `message:<saved sequence>`, or `interview:<answered bx question ID>` from the originating planning run. Validation suggestions may be empty; they do not become required project checks.

This validates structure and traceability, not whether the proposed solution is correct or whether cited text actually supports each claim. Model quality, contradictions, and task sizing still need human review and future evaluation fixtures.

## Saving and revising

**Save proposal as plan version** imports only an `anvil-plan` artifact from a successful current planning task associated with this goal and its current revision. It does not approve the run or require verification to have finished: check results remain visible in the run. An artifact from a failed, unfinished, superseded, unrelated, or older-goal planning attempt cannot be imported.

The visual artifact displays phases, expandable assignments, requirements/examples, assumptions, exclusions, open questions, and provenance. **Edit plan** opens a form for the latest version's title, summary, task instructions/reasoning, dependencies, requirement assignments, acceptance criteria, suggested validation, requirement descriptions/examples, and planning caveats. List items retain multiline text. The form adds/removes phases, tasks and requirements, moves tasks between phases, and selects saved-context citations. IDs stay stable within an edit. Removing a task removes its dependency links; referenced requirements and occupied phases cannot be removed. The coverage view distinguishes assignment from verified completion. Adjacent versions show additions, removals, ordering and full changed content. **Revise plan JSON** and **Import plan JSON** remain available without a model run. Saving appends an immutable version; previous content and digests remain available. Editing a model-derived version retains its source artifact as lineage, while the edited version has its own bytes and digest. A new goal revision marks old plans as earlier context. Updating a plan does not increment the goal's context revision.

Edits use expected goal and plan versions. The server still validates references, requirement coverage and dependency cycles. A concurrent edit fails without replacing either version; the UI preserves the unsaved draft for reconciliation, including a read-only **Draft JSON for copying** disclosure. Required-field errors open the affected collapsed editor. Suggested validation edits do not change project check policy. Saving never launches implementation or alters active runs. Request keys make matching retries idempotent while their source context remains available. Refresh and inspect existing versions/runs after an ambiguous network result rather than starting a distinct request blindly. Unsaved drafts are local component state and do not survive navigation or reload.

Migration `0300_goal_plans.sql` adds append-only, tenant-scoped goal/run associations and plan versions. The browser RPCs all use `WorkflowService`:

| RPC | Important inputs | Result |
| --- | --- | --- |
| `StartGoalPlanning` | Goal/revision, request key, harness/model/effort, scope, runtime seconds, instruction/skill file paths | Frozen run |
| `GetGoalPlans` | Goal, optional `before_version` | 20 plan versions per page; 20 most recent planning/implementation runs |
| `SaveGoalPlan` | Expected goal/plan versions, request key, exactly one of JSON or evidence ID | New or matching existing version |

Reads require an authenticated tenant session. Writes require CSRF and a live owner/admin/member session. Project-scoped machine and MCP clients can use the same goal APIs; see [machine API and MCP](machine-api-and-mcp.md).

## Validation and next boundary

Unit and PostgreSQL checks exercise the plan schema, frozen platform inputs, artifact hashes, run/goal association, completed/current evidence, immutable history, version conflicts, live access rules, and tenant isolation. Browser API tests exercise CSRF and offline dispatch refusal. The development mock supports model selection, an interactive follow-up, artifact import, and revisions without provider calls.

Selected plan versions can now be [compiled, previewed, and explicitly launched for implementation](anvil-execution.md). Live model/AX behavior remains unqualified by these tests. Automatic goal continuation, model recommendations, comprehensive usage budgets, and independent per-task workers remain separate work.

Planning settings support [explicit repository rules and skills](anvil-execution.md#repository-rules-and-skills). `StartGoalPlanningRequest.instruction_files` and `skill_files` populate the planner profile before freezing; execution uses the same fields on `GoalExecutionOptions`. Empty selections retain automatic applicable `AGENTS.md` inputs.

## Decisions, unknowns and readiness

Planning prompts now adapt across outcomes, concrete behavior, constraints, technical decisions, quality, and resource preferences. They tell the planner to inspect repository facts, skip answered/immaterial topics, explain recommendations, and reconcile contradictory answers through a focused question. No fixed sequence of rounds is mandatory, and no recommended option or silence is treated as an answer. The interview still uses the durable native question protocol; model interview quality requires live qualification.

Plans optionally contain `decisions` and `unknowns`, up to 32 each. Decisions record stable ID, question, choice, concise reason, `authority` (`user` attribution or `proposal`), saved sources, and optional considered alternatives. Unknowns record stable ID, description, reason, saved sources and a disposition: `blocking`, `investigable`, `assumed`, or `deferred`. Repeated normalized decision questions, invalid classifications, and citations to unsaved/unanswered context are rejected. Validation checks structure and available citations; it does not certify that a claimed attribution faithfully interprets the cited text.

The visual editor supports adding/editing/removing these records and selecting citations. Plan views show proposals separately from choices attributed to the user, classified unknowns, and a readiness summary. Versions and comparisons retain edits and provenance. Existing brief/context imports stay bounded; the original brief remains unchanged when plans are revised.

Execution settings add a separate **Planning readiness** selector: off, advisory (default), or required. Required blocks compilation when the saved plan contains a classified blocking unknown or an unclassified open question. Investigable/assumed/deferred items remain visible. Off/advisory permit launch with those declarations, while normal runtime questions can still be necessary. The selection and unresolved IDs are frozen in `anvil.readiness/v1alpha1` alongside the selected plan. This never changes verification gates, approves proposed decisions, or proves implementation quality.

## Plan-change impact

The visual editor shows an unsaved change-impact preview before saving. Version comparisons show the same analysis. Changed requirements flag their assigned tasks in both versions, changed phases flag their members, task edits flag that assignment, and impact follows downstream dependencies through both graphs. Shared planning context changes conservatively flag every assignment. Removed tasks remain visible in the impact list. Cyclic drafts do not hang the preview; saving still requires server validation.

The preview is a structural review aid, not a semantic code analysis or evidence invalidation engine. It links recent runs for the edited plan so users can inspect candidates and decide whether to continue, pause or cancel. Saving creates immutable new plan bytes; existing runs, evidence and historical acceptance remain bound to their original version. No active worker silently adopts a new plan. Raw JSON edits still receive server validation and a version comparison after save; their incomplete drafts are not interpreted by the visual preview.

Real browser E2E edits an upstream task, observes downstream impact, reverses the edit to clear it, and verifies an intervening goal revision preserves the unsaved draft while blocking a stale save.
