# Interactive agent sessions (MVP contract)

Goal: from the web UI, start a Guild recipe run, watch every stage's agent live
in a real terminal, take over any stage interactively, hand back, and reach
human review. This file is the shared contract for the parallel work lanes.

## Guest side (inside the AX Task)

- Runner image adds `tmux` and `script` (util-linux).
- Socket: `/tmp/blaxsmith/tmux.sock`. Session: `agent`. Window 0, pane 0.
- The worker launches the harness autonomously in its existing noninteractive
  mode inside the pane:

  ```
  tmux -S /tmp/blaxsmith/tmux.sock new-session -d -s agent -x 200 -y 50 \
    -- /usr/local/bin/blaxsmith-tool-worker pane -- <harness argv>
  ```

  `pane` runs `<harness argv>`, tees raw stdout to
  `/tmp/blaxsmith/events.jsonl`, renders a readable view to the pane, records
  the harness native session id to `/tmp/blaxsmith/session-id`, writes the
  exit code to `/tmp/blaxsmith/exit`, then `tmux wait-for -S blaxsmith-done`.
- The worker blocks on `tmux wait-for blaxsmith-done`, then reads `exit` and
  `events.jsonl` and continues the existing result/exit-readback path.
- Session persistence flags (`--no-session-persistence`, `--ephemeral`) are
  dropped so native resume works. Session data stays in the attempt's temp home.
- Takeover: `tmux send-keys -t agent C-c`, wait for the harness to exit, then
  `tmux respawn-pane -k -t agent -- blaxsmith-tool-worker pane --interactive -- <resume argv>`.
  Resume argv per harness (verify against pinned versions):
  - Claude Code: `claude --bare <same scoped flags> --resume <id>`
  - Codex: `codex resume <id>` with the same config/disable flags
  - OpenCode: `opencode --session <id>` with the same config
- Handback = the human exits the native TUI. `pane --interactive` then signals
  `blaxsmith-done` exactly like the autonomous path. Human edits go through the
  same checks and review as agent edits.

## Platform → guest

- Tasks for interactive-capable stages set AX `spec.debug: true` so guest
  services are reachable. ponytail: debug exposes the whole guest process API;
  the connector is the only permitted caller (network policy), and a narrower
  AX guest API replaces this before multi-tenant release.
- Blaxsmith dials the guest via `atenet-router` in-cluster with gRPC metadata
  `ate-target-actor: <atespace>/<actor>` using the public
  `github.com/agent-substrate/env/proto/ateenv/v1alpha` clients. Never
  kubeconfig or port-forward.
- Attach = `StartProcess{command: ["script","-qfc","tmux -S /tmp/blaxsmith/tmux.sock attach -t agent [-r]","/dev/null"], stdin: true, env: {TERM: xterm-256color}}`,
  `StreamProcessOutput{follow:true}` → browser, browser → `WriteProcessInput`.
  `-r` (read-only) unless the viewer currently holds control.
- Resize = separate exec `tmux -S … resize-window -t agent -x <cols> -y <rows>`
  (controller only; viewers never resize).

## Browser ↔ Blaxsmith

WebSocket `GET /api/terminal/attempts/{attemptId}` (same origin, session
cookie, Origin check, current project/attempt authorization on connect and
revocation).

- Binary frames: terminal bytes, both directions. Server drops client bytes
  unless the connection's principal holds control.
- Text frames (JSON):
  - client → server: `{"type":"resize","cols":120,"rows":40}`
  - server → client: `{"type":"state","control":"agent"|"human","holder":"<principalId>"|null,"stage":"<stageId>","attemptStatus":"<status>"}`
  - server → client: `{"type":"exit","code":0}` then close.
  - server → client: `{"type":"error","message":"<safe message>"}` then close.

Connect RPCs (in the existing workflow service):
- `TakeOverAttempt(attempt_id)` → holder becomes caller, guest takeover runs,
  state frames broadcast. Only one holder; a second request fails
  `FailedPrecondition`. Requires the run-control permission, not just view.
- `HandBackAttempt(attempt_id)` → sends native exit to the TUI; the pane
  signals done; control returns to `agent` state until the attempt completes.
- Control holder is persisted on the attempt row, with a generation fence, so a
  browser reconnect cannot create a second controller.

## Flow (Guild recipe)

- Starting a run from a recipe freezes it and creates all stage tasks. Stages
  become ready when `depends_on` are accepted.
- When an attempt completes and is accepted, dependents become ready. The
  downstream frozen prompt gets a bounded handoff: upstream stage ids, their
  final summaries (last result message), and the resulting revision.
- `human_review` is not dispatched. The UI uses the EXISTING
  `GetCurrentReview` / `DecideReview{run_id, package_id, idempotency_key,
  action: approve|request_changes, feedback}` RPCs (already on main). The
  engine turns a `request_changes` decision into a correction attempt on the
  implement stage, bounded by `max_correction_cycles`. Do not add
  ApproveRun/RequestChanges.
- Deferred: MCP, recipe experiments, amendments, OIDC.

## Structured interactions (interviews, gates, escalations, loops)

Guild's human touchpoints: interviews (Forge R2: one question at a time,
options plus free text, rounds until "done/finalize"), approval gates (crew
plan go/revise/cancel, Forge mode confirm), escalations (blocked worker →
choose remedy), and loops (Foundry verify-fix cycles, `max_cycles`, halt with
reason, cap rewrite mid-run). The terminal can always answer these natively,
but the product needs them structured, durable, auditable, and
harness-agnostic.

### Guest shim (`bx`, a symlink to `blaxsmith-tool-worker`)

Available on PATH in every Task; state lives under `/tmp/blaxsmith/ix/`.

- `bx ask --json '<Interaction>'` blocks until answered and prints the Answer
  JSON on stdout. Exit 0 = answered; exit 3 = cancelled/stopped (the agent must
  stop the current step).
- `bx event --json '{"type":"phase"|"cycle"|"handoff"|"finding"|"progress", ...}'`
  appends a progress event and never blocks.
- `bx inbox` prints and consumes pending steering messages as JSON lines
  (`instruction`, `pause`, `halt{reason}`, `set_max_cycles{n}`). Agents call it
  at safe points: between loop cycles and before each phase.
- Harness wiring: the adapter injects an instruction block telling every harness
  to use `bx ask` for all human questions and `bx inbox` / `bx event` at loop and
  phase boundaries. For Claude Code, `AskUserQuestion` is disallowed in
  autonomous mode so questions go through `bx ask`. In a native takeover TUI
  the native widget works as normal.
- Guild skills and prompts that say "use AskUserQuestion" are satisfied by
  this mapping. Recipes don't need forked prompts.

Interaction:
```json
{"id":"<ulid>","kind":"question"|"approval"|"escalation"|"interview_round",
 "title":"...","body_md":"... evidence and context ...",
 "options":[{"id":"a","label":"...","description":"...","recommended":true}],
 "multi_select":false,"allow_free_text":true,
 "blocking":true,"sources":[{"path":"...","line":12}],
 "interview":{"round":3,"finalize_option":"done"}}
```
Answer: `{"interaction_id":"...","option_ids":["a"],"text":"...","answered_by":"<principalId>","at":"<rfc3339>"}`

### Platform side

- A per-attempt watcher (bounded, owned like the other loops) runs
  `bx watch` over the guest exec connection. That streams new interactions and
  events as JSON lines, and the watcher persists them. Delivery is `bx answer
  <id> --json '<Answer>'` and `bx steer --json '<msg>'` via guest exec.
- DB: `interactions` (attempt, stage, kind, payload, state
  open|answered|cancelled|superseded, answer, answered_by, timestamps).
  Progress events go into the EXISTING durable `workflow_events` log (new event
  types) so the existing activity hub carries them. No `attempt_events` table. Answer is idempotent per interaction
  (first answer wins; later ones get `FailedPrecondition`). Answers persist
  before delivery; undelivered answers are redelivered on watcher reconnect.
- Connect RPCs: `ListInteractions(run_id)`, `AnswerInteraction(interaction_id,
  option_ids, text)`, `SteerAttempt(attempt_id, kind, text|reason|n)`.
  Live updates: the EXISTING SSE `GET /api/runs/{runID}/events`
  (cmd/blaxsmith/run_activity.go, already authenticated with replay). Add
  event types `interaction.opened`, `interaction.answered`, `attempt.progress`
  and `attempt.control`. Do not build a second stream.
- The interview transcript for a stage is rendered from its interaction
  records and feeds `guild.Validate(spec, transcript)` for Forge-style specs.

### Recipe loops

- Stage field `"loop": {"with": "<stage id>", "until": "pass", "max_cycles": N}`
  on a verify/review stage. On failure, create a correction attempt on the
  `with` stage carrying the findings in its handoff, then rerun this stage.
  When the cap is hit, raise an `escalation` interaction with the options
  raise cap / accept with exceptions / halt (take over stays available from the stage terminal).
- In-agent loops (Foundry inside one stage) surface through `bx event cycle` and
  are steered through `bx inbox` (`set_max_cycles`, `halt`, `pause`).

### UI surfaces

- Run page **inbox**: open interactions across all stages, oldest blocking
  first. Each renders as a question card (options, recommended badge, free
  text, sources, evidence markdown).
- **Interview view** for interview stages: chat-style transcript of rounds with
  the current question card pinned and a "Finalize" action.
- **Loop panel** per loop: cycle timeline (cycle N / cap), findings per cycle,
  and controls for pause after this cycle, halt with reason, change cap, and
  add an instruction for the next cycle.
- Stage activity feed from `event`s; the terminal stays available alongside.

### Wire shapes (settled)

- `WorkflowEvent` gains `string payload_json = <next>` (proto JSON
  `payloadJson`, SSE JSON `payloadJson`). It's empty for existing types. For
  `attempt.progress` it holds the raw `bx event` object:
  `{type: phase|cycle|finding|handoff|progress, cycle?, max_cycles?, status?, text?|message?|name?}`.
  The server validates it as a JSON object capped at 8 KiB, and truncates
  text fields rather than rejecting. `interaction.opened`,
  `interaction.answered` and `attempt.control` carry no payload; clients refetch.
- `Interaction` proto (proto JSON camelCase): id, attemptId, stage, kind,
  title, bodyMd, options[{id,label,description,recommended}], multiSelect,
  allowFreeText, blocking, sources[{path,line}], interview{round,finalizeOption},
  state, answer{optionIds,text,answeredBy,at}, createdAt.

## Code flow between stages (decided: the platform pushes a run branch)

- On a normal finish the guest `pane` commits workspace changes as one commit
  (fixed bot identity) on top of the frozen commit, writes
  `/tmp/blaxsmith/result.bundle` (`git bundle create … <frozen>..HEAD`, size
  bounded) and `/tmp/blaxsmith/result.json`
  (`blaxsmith.attempt-result/v1alpha1`: summary, revision, verdict).
- Blaxsmith reads the bundle through guest `ReadFile` before stop, and checks
  that its tip equals `result.json.revision` and that its parent is the stage's
  input commit. It then pushes to `refs/heads/blaxsmith/run-<runId>` in the
  project repo using the project's Git connection, with a force-with-lease on
  the expected previous tip. Agents never hold write credentials.
- A downstream stage's input commit = the accepted upstream revision. For a
  fan-in with more than one upstream revision, the first proof is linear only:
  pick the single code-producing upstream and have reviewers read that
  revision. Its AX Workspace fetches that commit from the run branch.
- Final approval publishes the PR from the run branch (existing P1-08 path).
- Deferred: merging parallel code-producing stages, and branch cleanup policy.
