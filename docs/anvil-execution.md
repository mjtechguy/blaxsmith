# Execute a saved Anvil plan

In **Project → Goals**, select a saved plan version and expand **Prepare implementation**. Choose the implementation model, harness, effort, repository scope, seconds per stage, correction cycles, and acceptance mode. Then select **Preview implementation**.

The preview shows the pinned repository commit, execution stages, frozen task packets, selected project checks, input digests, and launch blockers. Expand each packet to inspect its exact JSON instructions, dependencies, requirement examples, phase outcome, and goal/plan identity. **Launch implementation** uses that preview; changing settings requires another preview. No worker starts during preview.

## Execution shape

The current engine supports one code-writing stage per run. Anvil therefore compiles the complete selected plan into dependency-ordered packets consumed by **one implementation worker**, followed by the existing platform verification stage. Packets do not represent separate workers, independently tracked task completion, or a parallel scheduler.

The worker receives the pinned goal context, answered planning questions when the plan has a source run, exact selected plan bytes, task packets, and applicable repository instructions. It is instructed to inspect existing code and tests, follow task dependencies, ask about unresolved decisions, report progress with task IDs, and publish a final report. The platform owns candidate-branch delivery and verification. Suggested tests in a task remain suggestions; they do not silently become required checks.

Implementation produces a candidate run branch. It does not merge or push the target branch. The goal workspace links to the existing run interface for actual stage status, questions, steering, evidence, and acceptance.

## Follow a running factory

The run overview and Stages tab include an interactive **Factory map**. Columns follow the frozen dependency graph; independent stages share a column. Select a stage to open its existing work log, questions, steering and terminal controls. Dependency and repair-target links open the corresponding stage. The selected stage is stored in the URL and survives reload.

Cards show recorded stage state, reserved attempt count, and repairs requested against the current cap, including user-granted increases. Platform verification is identified separately from model work. The acceptance card opens candidate checks and artifacts and distinguishes execution completion, a pending evidence package, human acceptance, and policy acceptance. A prior package cannot appear accepted while a run has reopened. This is stage-level execution data, not per-packet completion or proof of a merge.

The map uses existing engine APIs and live events and works with Anvil, Guild, and custom recipes. Recovered events refresh stage, run, interaction, evidence, and acceptance data. On small screens columns stack vertically. Keyboard users can open stage links and navigate the scrollable map. No graph dependency, new API, or factory-specific execution path is added.

## User controls

- **Acceptance:** human review or policy acceptance. Policy acceptance requires the selected requirements to pass without adding a human approval gate. An empty required-check set does not establish tested correctness.
- **Correction cycles:** 0–10 automatic implementation repairs after required verification fails. Zero is the default and leaves the failed check blocked. A positive value freezes a verify → implement repair loop: findings reach the next implementation attempt, which starts from the previous candidate, then verification runs again. Exhausting the loop escalates for a user decision. Failed advisory checks stay visible without triggering repair. The same setting also controls the engine’s base retry allowance; interrupted attempts and explicit human corrections can add attempts, so this is not a total run-attempt cap.
- **Runtime:** 60–3600 seconds per stage attempt. Retries can multiply total runtime. These controls are not dollar or token budgets.
- **Project checks:** existing required/advisory/off modes are frozen unchanged. Verification uses the platform runner, without a second model invocation.

The model picker uses connected account catalogs. Launch independently checks project model grants, runtime readiness, and source configuration. Implementation needs a configured Git connection suitable for writing its candidate branch. There are no hardcoded model rankings or automatic upgrades.

## Repository rules and skills

Expand **Repository rules and skills** in planning or implementation settings. Add repository-relative instruction paths for stack conventions, architecture boundaries, testing requirements, or agent roles. The file lookup suggests Markdown documents and `SKILL.md` entry points from the project's configured source and shows the observed commit. Exact paths can also be entered. Discovery happens only when the section is opened; a lookup failure does not erase selections.

Applicable `AGENTS.md` files remain automatic. Planning and implementation selections are separate and start empty. Add each skill's `SKILL.md` and any required support files under the same directory; only explicitly selected files are installed for native harness discovery. Skills do not confer tool permissions. These controls select committed files, not arbitrary local files, URLs, uploads, or executable plugins.

The selected profile uses the existing recipe `instructions` and `skills` fields. Freezing reads from the pinned Git commit, records bytes/digests, and rejects unavailable files. The implementation preview lists frozen files; changing a selection requires another preview. Active runs retain their original context. File suggestions are advisory and can be older than the source fetched during preview or launch. Shared launch preflight validates portable skill frontmatter, unique directory names, and explicitly selected support files before admitting a run. The worker repeats these checks against its checkout.

## Durable compilation and admission

`internal/anvil/execution.go` owns native compilation. The engine retains ordinary factory-neutral recipes, task dispatch, checks, and acceptance. Guild is not a dependency.

Each `anvil.task/v1alpha1` packet contains a task's full instructions, dependencies, acceptance and suggested validation, its phase, and only its referenced requirements/examples. It also records the goal ID/revision and selected plan version/content digest. Dependencies are topologically ordered, preserving plan order where dependencies allow. The complete plan and current goal context remain shared inputs, so unrelated constraints are not silently lost.

Platform inputs use the reserved `.blaxsmith/platform/` namespace and explicit provenance. Packets are frozen documents delivered inline to the worker, not files claimed to exist in Git. Git collisions are rejected. Compilation rejects oversized inputs rather than truncating: context at most 256 KiB, native execution files at most 768 KiB, and at most 900 KiB after combining repository instructions. These compilation ceilings do not guarantee that the worker request fits: its JSON-encoded command argument has a stricter 120 KiB limit, including escaping, manifests, and runtime metadata.

Preview and launch assemble each non-human stage's initial worker request through the same builder used by dispatch. An oversized request or malformed selected skill produces a stage-specific blocker before run admission. Reduce instructions or split large goals; automatic context compaction is not implemented. Future stage handoffs and final runtime bindings are not available during preview, so dispatch rechecks the complete request before execution. Passing preview does not reserve capacity or guarantee future admission.

Migration `0310_goal_execution.sql` adds an optional plan-version foreign key to the immutable goal/run association. Frozen run admission checks the live goal revision and exact selected plan digest in the same transaction that creates and seals the run. The selected plan may be an earlier version if it still uses the current goal context; the preview names that version explicitly. A plan from an earlier goal revision must be revised first.

Launch requires matching preview bundle and verification hashes. Changed source commits, execution settings, generated inputs, or project checks invalidate the preview. Existing role, session, source, and model checks still apply. A matching launch key is idempotent; it cannot be reused to change the plan association. If a network outcome is uncertain, inspect linked runs before issuing a different key.

## Browser API

This uses the existing `WorkflowService.PreviewRun` and `LaunchRun` RPCs. Both require browser-session CSRF and launch authority. `goal_execution` is mutually exclusive with `recipe_path` and `recipe_version_id`.

Example preview payload (IDs are illustrative):

```json
{
  "projectId": "<project-uuid>",
  "scope": ".",
  "goalExecution": {
    "goalId": "<goal-uuid>",
    "expectedGoalRevision": "3",
    "planVersion": "2",
    "harness": "codex",
    "model": "<authorized-model-id>",
    "effort": "medium",
    "runtimeSeconds": 900,
    "correctionCycles": 0,
    "acceptance": "manual"
  }
}
```

`PreviewRunResponse.task_packets` returns the exact compiled assignment JSON, paths and SHA-256 values. Launch sends the same options and scope, a stable `launch_key`, and both returned `expected_bundle_sha256` and `expected_verification_sha256` values. Preview readiness is advisory; launch rechecks it.

`GetGoalPlansResponse.runs` returns the 20 most recent goal-associated runs. `plan_version = 0` denotes planning; a positive value identifies the executed plan version. Full run history remains in the platform's run interface. Machine-token and MCP authorization are described in [machine API and MCP](machine-api-and-mcp.md).

## Validation and remaining qualification

Compiler tests exercise ordering, scoped requirements, stable packet digests, optional gates, bounded inputs, and real Git freezing. PostgreSQL tests exercise plan identity, immutable run association, idempotent launch, conflicting plan keys, and stale context. Browser API and client tests cover CSRF, source exclusivity, project boundaries, and required preview pins. The local mock supports preview/launch and retained goal-run links without provider calls.

The local regression `TestAnvilExecutionCorrectionPostgres` compiles a saved-plan fixture, freezes it from real Git, records authenticated simulated worker exits in PostgreSQL, delivers real Git bundles to a temporary bare candidate branch, and executes a real shell check. It covers disabled repairs, one successful repair under manual/policy acceptance, exhausted repair escalation, and advisory failure without repair. It checks feedback handoff, candidate continuity, acceptance provenance, and an unchanged target branch. AX actors and model work are simulated; the shell check runs locally, not in the production verifier sandbox.

Run it with an existing isolated-test database:

```sh
BLAXSMITH_TEST_DATABASE_URL='<test database URL>' go test ./cmd/blaxsmith -run TestAnvilExecutionCorrectionPostgres -count=1
```

Live AX/model execution remains unqualified. On 2026-09-26, the documented dev node rejected this session’s noninteractive SSH authentication, before any remote command or deployment ran. Independent per-task workers, verified task-level progress, automatic continuation, and comprehensive resource accounting remain separate work. The next useful qualification is an authorized small real repository run through implementation, checks, correction, and acceptance.

Independent review can now be Off, Advisory or Required with a separate role profile. See [review evidence](review-evidence.md#independent-native-review) for its report contract and qualification limits. [Delivery snapshots](delivery-export.md) export the frozen checks, acceptance and evidence index.
