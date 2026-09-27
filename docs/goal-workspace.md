# Goal workspace

A goal owns a brief, planning context, and structured decisions before a run or attempt exists. Open **Project → Goals** to create an Anvil goal, answer its optional starter questions, defer questions, or add context. Reloading restores the saved conversation and current answers.

The workspace now supports an explicit Anvil planning run: select a model, harness, effort, repository scope, and per-stage time limit. The planner receives frozen goal context and a pinned repository, can ask follow-up questions, and publishes a structured proposal. Save the proposal to inspect its phases, tasks, reasoning, examples, and source references. JSON edits append a new version. See [Anvil planning](anvil-planning.md) for the contract and limitations. Planning preferences do not alter project verification policy. Implementation requires a separate [preview and launch](anvil-execution.md) of a selected saved plan.

## Ownership and data

The factory-neutral store is in `internal/interact/goals.go`. It reuses the existing interaction shape, option/answer validation, tenant scope, and live-session mutation checks. Anvil supplies its starter questions from `internal/anvil/interview.go`; only the application composition layer chooses that factory. The persistence and task engine do not import Anvil or Guild. The browser creation entry point currently initializes Anvil; other factory initialization is not yet exposed.

Migration `0290_goal_workspace.sql` adds tenant-scoped goals and append-only entries. The original brief, factory identity/version, and initial questions are immutable. Additional context and revised answers are new entries. Reading a goal returns current decisions plus a chronological history page from one consistent database snapshot. Questions that have been deferred remain answerable.

A goal revision increments for each saved message, answer, or deferral. Every mutation supplies the revision the user saw. Concurrent edits have one winner; the other receives a conflict and must review current decisions before retrying. Revision history preserves earlier answers instead of overwriting them.

## Browser API

All endpoints use `WorkflowService` and derive the organization from the authenticated browser session. Owners, admins, and members can create and change goals. Viewers can read them. Writes require CSRF and recheck the live session and role under database locks. Project-scoped machine credentials and MCP use the same stores through a separate authenticated endpoint; see [machine API and MCP](machine-api-and-mcp.md). External factory intake and versioned plans are supported without Anvil interpretation.

| RPC | Inputs | Result |
| --- | --- | --- |
| `CreateGoal` | `project_id`, `request_key`, `title`, `brief` | Goal with Anvil starter questions; no source or model setup required |
| `ListGoals` | `project_id`, optional `before_id` | Up to 50 goals, newest first, and `next_before_id` |
| `GetGoal` | `goal_id`, optional `before_sequence` | Goal/current answers, up to 50 chronological entries, and `next_before_sequence` |
| `ReplyGoal` | `goal_id`, `request_key`, `expected_revision`, `kind`, optional question/answer fields | Durable mutation; fetch the current goal afterward |

Reply kinds are `message`, `answer`, and `deferred`. Answers may use stored `option_ids`, free text when allowed, or both. Deferral is accepted only for optional questions. Ordinary context messages cannot masquerade as answers or worker instructions. Titles are limited to 160 characters, briefs to 16,000, and replies to 4,000. The initial interview supports up to 16 questions.

Request keys are scoped to the actor and goal (or project for creation). Retrying the same payload with the same key is idempotent, including when its original response was lost. Reusing a key for different content fails. Keep the key until the outcome is known; use a new key for a distinct contribution.

## UI and validation

The workspace presents the original brief and history alongside selectable question cards. It reuses the accessible live-run question form without importing the terminal bundle. Decisions can be revised, optional questions deferred, and older history loaded. The phase strip describes the factory journey; it does not claim execution progress. The planning panel shows actual run state, questions, artifact versions, and stale-context indicators. The latest saved plan can be edited through labelled task and requirement forms; structural changes remain available through JSON. Each save creates a new version and preserves earlier assignments.

Database tests cover independent goal ownership, idempotency, stale and concurrent edits, retained answers, pagination, immutable history, tenant boundaries, viewer denial, and revoked sessions. Browser API tests cover CSRF, creation/readback, stale replies, project-scoped listing, plan validation/version conflicts, and refusal to launch without a dispatcher. The development mock supports the same visual workflow; its in-memory data is for UI testing only.
