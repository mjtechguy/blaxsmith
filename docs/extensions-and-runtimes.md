# Extension packs, runner images, and harness adapters

Status: decided direction (2026-09-24). Phase 1 ("Guild embedded") is being
built on `lane-k-extensions`; later phases are contracts only.

This document covers four connected decisions:

1. **Extension packs**: versioned, admin-installed packages of workflow
   content (stage templates, skills, agents, MCP servers, hooks, UI hints).
2. **Integration modes**: *embedded* (the extension's own orchestrator runs
   inside one stage) and *decomposed* (a recipe of platform stages), plus the
   `bx` bridge that connects an in-sandbox orchestrator to the platform.
3. **Runner images**: a small pool of layered, digest-pinned images selected
   per run.
4. **Harnesses**: a capability model, dedicated adapters, and a generic ACP
   adapter.

It extends [the plan](../agent-factory-plan.md) sections 4 (composable teams,
tool releases, recipes), 6 (Guild migration), and 8 (skills, tools, hooks/MCP
policy), and tasks P1-20, P1-24, and P1-28. Where this document conflicts
with the plan's "no marketplace" and "native delegation disabled" rules, the
exceptions are narrow and stated in [Trust rule](#trust-rule) and
[Embedded mode](#embedded-mode).

## 1. Extension packs

An **extension** is a Git repository (or a directory within one) plus a
manifest, `blaxsmith-extension.json`, that describes what the platform may
take from it. An **extension version** is one immutable installation of that
extension at one Git commit.

### Lifecycle

1. **Install.** An organization owner or admin names a Git URL and a ref
   (branch, tag, or commit). The platform resolves the ref to one commit,
   fetches that commit through the existing public/private Git fetch path
   (`internal/gitfetch`, the same proxy and credential rules as project
   sources), and reads the manifest there. A repository that does not carry a
   manifest (for example Guild today) can be installed with an **overlay
   manifest** that the admin supplies; the overlay is validated against the
   fetched tree exactly like an in-repo manifest, and its origin is recorded.
2. **Validate.** The manifest must match the JSON Schema
   ([`extension-manifest.schema.json`](extension-manifest.schema.json)) and the
   semantic rules in `internal/extension`: every referenced path exists as a
   regular file or directory at the commit, imported Claude Code plugins have
   `.claude-plugin/plugin.json`, hooks and MCP servers named in the manifest
   exist in those plugins, and no path escapes the repository.
3. **Approve.** The platform derives the **declared permissions** from the
   manifest: each hook, each MCP server, each egress host, native subagent use,
   and each required runtime. The install page shows them in a confirmation
   modal. The admin approves them; permissions the manifest marks `optional`
   (for example a convenience hook) may be declined. Install fails if a
   required permission is not approved. The approved set is stored with the
   version and hashed.
4. **Grant.** Installing does not make an extension usable. Projects,
   principals, or roles get use through `access.CanUse` with resource kind
   `"extension"` (the organization resource grants from lane U).
5. **Use.** A recipe stage references `extension@version/template`. Freezing
   a run resolves the reference to one installed version, checks the grant,
   and records the version, commit, manifest digest, and approved permission
   digest in the bundle, so a run never picks up a later install.
6. **Update.** The platform periodically (and on demand) resolves the stored
   ref again. When it points to a different commit, the extension shows
   "update available". Updating is a new install at the new commit; nothing
   changes existing versions, recipes, or runs.

Versions are immutable rows (a trigger rejects UPDATE and DELETE), like
recipe versions. `(extension, version)` is unique; reinstalling a manifest
version string at a different commit is refused, so `guild@1.0.0` always
means one commit. Only internal authors are supported for now: there is no
signature check, publisher verification, or marketplace. The install is an
admin decision recorded in the audit log.

### Manifest

`blaxsmith-extension.json` (schema id `blaxsmith.extension/v1alpha1`):

| Field | Meaning |
|---|---|
| `schema_version` | `"blaxsmith.extension/v1alpha1"`. |
| `id` | Stable identifier, `^[a-z][a-z0-9-]{0,63}$`. Unique per organization. |
| `version` | Semantic version of the pack (`MAJOR.MINOR.PATCH`, optional pre-release). |
| `publisher` | `{name, url?}`. Informational until signing exists. |
| `description` | Short text for the admin UI. |
| `harnesses` | Supported harnesses: `[{harness, min_version?, capabilities: [...]}]`. `capabilities` lists what the pack *requires* from that harness (see [Capability model](#capability-model)). |
| `runtimes` | Runner runtime layers needed in the sandbox: `[{id, version}]` (for example `uv`, `python`, `serena`). Selection maps them to a runner image layer (section 3). |
| `egress` | Hosts the sandbox must reach: `[{host, port, purpose}]`. Added to the attempt Gateway only when approved. |
| `plugins` | Claude Code plugin imports: `[{id, path}]`. `path` is a directory containing `.claude-plugin/plugin.json`. The plugin's commands, agents, and skills are imported as-is; its hooks and MCP servers are imported only as declared below. |
| `stage_templates` | `[{id, title, mode, harness, kinds, plugins, prompt, entrypoint?, skills?, rules?, agents?, mcp_servers?, hooks?, interactions?, signals?}]`. A template is prompt + skills + agents + tools for one stage. `mode` is `embedded` or `decomposed`. `kinds` are the recipe stage kinds it may fill. `prompt` (at most 8 KiB) is prepended to the stage's frozen prompt; `{{plugin_root:<plugin id>}}` is replaced with that plugin's materialized directory. `mcp_servers` and `hooks` name top-level entries the template activates (when approved). `signals` map MCP tool calls to `bx event`s: `{plugin, server, tool, event: phase\|handoff\|cycle\|progress, name_field, text_field?}`. |
| `recipes` | Flows the pack contributes: `[{id, path}]`, a recipe JSON file in the pack (decomposed mode, Phase 3). |
| `skills` | Portable Agent Skills outside plugins: `[{path}]` to a `SKILL.md` directory. |
| `rules` | Instruction files: `[{id, path, applies_to}]`. |
| `agents` | Subagent definitions outside plugins: `[{id, path}]`. |
| `checks` | Gates the pack can report through `bx gate` (Phase 2): `[{id, title, verdicts}]`. |
| `interactions` | Interaction templates: `[{id, kind, title}]`, the `bx ask` kinds the pack raises. |
| `mcp_servers` | `[{id, plugin?, server?, transport, url?, optional, purpose}]`. `plugin` + `server` name a server declared in that plugin's `plugin.json`; `transport: "http"` with a loopback `url` names a server the pack starts inside the sandbox. Always sandbox-only. |
| `hooks` | `[{id, plugin, event, command, optional, purpose}]`. Must match the plugin's `hooks/hooks.json`. Sandbox-only, off unless approved. |
| `ui` | `{phases: [{id, title}], artifacts: [{glob, renderer}]}`: phase names and how to render produced files. |
| `native_subagents` | `true` when an embedded template needs the harness's own subagents/teams (Foundry needs this). A permission. |

Unknown fields are rejected. Paths are repository-relative, clean, and
contain no `..`, backslashes, or control characters.

The complete Guild example is in
[`examples/extensions/guild/blaxsmith-extension.json`](../examples/extensions/guild/blaxsmith-extension.json)
and is reproduced in [Appendix A](#appendix-a-guild-manifest).

### Trust rule

- **The platform never runs extension code.** The control plane parses JSON
  and copies bytes. It does not execute scripts, hooks, MCP servers, build
  steps, or validators from an extension, at install or at any other time.
- **Extension code runs only in sandboxes**, inside the attempt's AX Task,
  under the same gVisor boundary, Gateway, and credential rules as the agent.
  An extension MCP server or hook has exactly the authority of the agent's own
  shell in that attempt. It never receives platform credentials; any access it
  needs goes through Connection → Grant → Binding → Lease like any tool.
- **Declared plus approved.** Only declared hooks, MCP servers, egress hosts,
  and native-subagent use can be active, and only if the admin approved them.
  The adapter drops an undeclared or unapproved hook or MCP server from the
  materialized plugin; it does not warn and continue.
- **Frozen like recipes.** A run freezes the extension version, commit,
  manifest SHA-256, and approved-permission SHA-256. The worker checks out the
  extension at the frozen commit and refuses to start if `HEAD` differs.
- **Exceptions to plan §8.** Native subagents stay disabled for platform
  stages. An embedded template that declares `native_subagents` and whose
  version was approved with that permission may use the harness's own
  subagents/teams *inside its one sandbox*. This is the "legacy Guild run
  inside one isolated environment" compatibility step in plan §6; it does not
  satisfy separately isolated worker acceptance. Decomposed mode and
  `bx spawn` (Phase 3) replace it.

## 2. Integration modes and the `bx` bridge

### Embedded mode

The extension's own orchestrator runs inside one recipe stage. For Guild that
is Claude Code running `/forge:plan` or `/foundry:start` with the plugins
materialized. The orchestrator keeps its methodology, prompts, validators,
and ledgers; the platform sees one stage attempt and whatever the orchestrator
reports through `bx`:

- **Questions.** `AskUserQuestion` is disallowed while autonomous. The
  adapter appends a mapping block to the existing `bx` instruction block:
  every place the extension's instructions say to call `AskUserQuestion`,
  the agent runs `bx ask` instead (question → `kind: question`; `header` →
  `title`; `options[].label/description` → `options[]`; `multiSelect` →
  `multi_select`; free text always allowed). The native widget still works
  after a human takeover.
- **Phases and handoffs.** The template declares `signals`: MCP tool calls the
  pane observes in the harness's structured output and turns into `bx event`
  records. For Foundry, a call to its `Foundry-Phase` tool becomes
  `{"type":"phase","name":"<phase>","source":"foundry"}` and a
  `Foundry-Handoff` call becomes `{"type":"handoff","name":"<event>","text":"<summary>"}`.
  They reach the UI as `attempt.progress` through the existing watcher and SSE.
  Only tool calls are observed; the pane never reads the extension's files.
- **Result.** The stage ends like any other: one squashed commit, a bundle,
  `result.json`, and a verdict from `bx event verdict` when the template is a
  review/verify kind.

### Decomposed mode

The extension contributes a recipe whose stages are platform stages, each
filled by a stage template (prompt + skills + agents + tools). The platform
schedules, isolates, and reviews each stage, and the extension's role
boundaries become stage boundaries. This needs recipe fan-out and conditional
edges (Phase 3) to express Foundry's waves and verify-fix loops without the
in-sandbox orchestrator.

### `bx` bridge roadmap and wire contracts

All `bx` commands are the `blaxsmith-tool-worker` binary, state under
`$BLAXSMITH_STATE_DIR/ix/` (see [interactive sessions](interactive-sessions.md)),
exchanged with the platform by the per-attempt watcher over guest exec. Exit
codes: 0 ok, 1 failure, 2 invalid, 3 cancelled/stopped.

**Existing (Phase 1):** `bx ask`, `bx event`, `bx inbox`, `bx watch`,
`bx answer`, `bx steer`, unchanged. `bx event` gains no new types; embedded
signals use `phase` and `handoff`.

**`bx gate` (Phase 2)**: platform-enforced, audited gates and verdicts.

```
bx gate --json '{"id":"<ulid>","check":"<manifest check id>","verdict":"pass|fail|blocked",
  "summary":"...","evidence":[{"path":"...","sha256":"..."}],"requirement_ids":["FR-001"]}'
```

- Blocks until the platform records it; prints
  `{"gate_id":"...","accepted":true|false,"reason":"..."}`. Exit 0 when
  accepted, 1 when the platform refuses (unknown check, evidence missing from
  the stage commit, digest mismatch), 3 when stopped.
- The watcher persists it as a `workflow_gate_results` row (attempt, stage,
  check, verdict, evidence digests, recorded_at) and a `gate.recorded` event.
  The check must be declared by the frozen extension version and required by
  the recipe to affect acceptance; otherwise it is informational.
- Unlike `bx event verdict`, a gate result is part of acceptance: a stage with
  a required check and a `fail` result cannot be accepted, and the record is
  audited.

**`bx artifact` (Phase 2)**: publish reports and ledgers to review.

```
bx artifact --json '{"id":"<ulid>","path":"foundry-archive/run-1/report.md",
  "kind":"report|ledger|log|image","title":"...","renderer":"markdown|json|table|image"}'
```

- The file must be inside the workspace, a regular file, at most 4 MiB, and
  is read by the platform through guest `ReadFile` at stage end (not live).
  Prints `{"artifact_id":"...","sha256":"..."}`. Exit 2 for a bad path or
  size.
- Stored with the stage result and shown on the review page with the
  manifest's `ui.artifacts` renderer. Artifacts are evidence, never
  instructions for later stages unless the recipe hands them off explicitly.

**`bx spawn` (Phase 3)**: an in-sandbox orchestrator asks the platform for a
sub-stage in its own sandbox and awaits the result.

```
bx spawn --json '{"id":"<ulid>","template":"guild@1.0.0/foundry-teammate",
  "harness":"codex","model":"gpt-5.6-luna","effort":"high",
  "prompt":"...","inputs":[{"path":"...","sha256":"..."}],
  "base":"<commit from this stage or HEAD>","timeout_seconds":1800}'
```

- Blocks; prints `{"spawn_id":"...","state":"succeeded|failed|refused|cancelled",
  "revision":"<commit>","summary":"...","verdict":"pass|fail|"}`. Exit 0 when
  it ran (check `state`), 1 when refused, 3 when this stage was stopped (the
  child is stopped too).
- The platform treats a spawn as a child task of the current attempt: it
  resolves the template in the parent's frozen extension set (no new
  versions mid-run), checks the harness/model against approved runtimes and
  the recipe's profiles, enforces RBAC (the run initiator must be able to use
  the model connection), per-run concurrency and depth limits (default depth
  1, at most 8 live children), and the run budget. A refusal returns a reason
  and is audited.
- The child's commit is pushed to the run branch under a child ref; the parent
  fetches it only through `bx spawn`'s returned revision. Children cannot
  spawn unless the recipe allows depth > 1.

## 3. Runner images

### Layers

| Layer | Contents | Changes when |
|---|---|---|
| Base | AX task runner, `blaxsmith-tool-worker` (pane, `bx`), tmux ≥ 3.2, `script`, Git, CA bundle | Worker or AX pin changes |
| Toolchain profile | One of `node`, `go`, `python`, `rust`, `jvm`, `polyglot`: the language toolchain a project's checks and setup commands need | Toolchain pin changes |
| Harness CLIs | Pinned Claude Code, Codex, OpenCode (the existing lockfile-verified set) | Tool release promotion (P1-24) |
| Extension runtimes | Per-runtime layers declared by extensions, e.g. `guild` = uv + Python 3.12 + Serena | Extension runtime pin changes |

Every layer is built by the **trusted image build** (outside agent pods, from
committed inputs with pinned digests and checksums), smoke-tested offline,
and recorded in a proof (`proof.json`) with its manifest digest. The build
never runs extension code except the pinned installers its own Dockerfile
names.

The `guild` extension-runtime layer is built today as
`deploy/tool-worker/build.sh ... --variant guild`
([tool-worker README](../deploy/tool-worker/README.md#runner-variants)). Its
pins live in `deploy/tool-worker/guild-runtimes.json`, and
`check-variant.py` refuses a build whose pins differ from the Guild
manifest's `runtimes`.

### Image pool and selection

The platform keeps a small pool of pre-built, digest-pinned combinations
(for example `node+harnesses`, `polyglot+harnesses+guild`), not one image per
run. Per run it selects the first pool image that satisfies:

1. the project toolchain: `.blaxsmith.json` when present, otherwise
   repository detection (`SetupService.InspectRepository`, lane S);
2. the recipe's harnesses (every stage profile);
3. the extensions' `runtimes` (every frozen extension version);
4. a project override (an admin-set pool image for the project), which must
   still satisfy 2 and 3.

The selected image digest is frozen with the run. No match is a visible
preflight blocker ("no runner image provides python + guild"), never a
fallback. Later: an admin-approved custom devcontainer profile builds a
project-specific image through the same trusted build.

### Required AX change

AX today accepts one `--blaxsmith-runner-image`. Selection needs AX to accept
an **allowlist of runner image digests** and to reject any Task whose image is
not on it. This is a separate, security-reviewed overlay patch in
`integrations/ax/`; Phase 1 does not change it, so Phase 1 extension stages
run only if the single allowed image is the `guild` variant.

## 4. Harnesses

### Capability model

| Capability | Meaning | Claude Code | Codex | OpenCode | ACP (generic) |
|---|---|---|---|---|---|
| `resume` | Native session resume in the takeover TUI | yes | yes | yes | if `session/load` or `session/resume` |
| `native_questions` | A structured question tool the adapter can disable and map to `bx ask` | yes (`AskUserQuestion`) | no | yes (`question`) | `session/elicitation`, `session/request_permission` |
| `subagents` | Native subagents/teams (off unless approved) | yes | yes (`multi_agent`) | yes | agent-defined |
| `skills` | Native Agent Skills discovery | yes | yes | yes | no (prompt only) |
| `mcp` | MCP servers from adapter-written config | yes | yes | yes | `session/new` `mcpServers` |
| `hooks` | Lifecycle hooks from plugin config | yes | no | plugins | no |
| `plugins` | Claude Code plugin directories | yes (`--plugin-dir`) | no | no | no |
| `structured_protocol` | Machine-readable event stream | stream-json | `exec --json` | `run --format json` | JSON-RPC `session/update` |

Recipes (per profile) and extension templates (per harness entry) declare
required capabilities. **Preflight** (recipe validation, freeze, and dispatch)
refuses an unsupported combination with a specific reason, for example
"stage `build` uses `guild@1.0.0/foundry-build`, which requires `plugins`
and `hooks`; profile `implementer` uses `codex`".

### Adapters

- **Dedicated adapters** stay for Claude Code, Codex, and OpenCode
  (`internal/tooladapter`): each owns argv, isolation flags, skill
  materialization, resume argv, and event parsing.
- **Generic ACP adapter** for new harnesses (Grok and others): the worker
  speaks the [Agent Client Protocol](https://agentclientprotocol.com) over the
  harness's stdio: `initialize` → `session/new` (cwd, adapter-written
  `mcpServers`) → `session/prompt`, reading `session/update` notifications for
  the work log, answering `session/request_permission` from policy (never a
  blanket allow), mapping elicitation to `bx ask`, and serving `fs/*` and
  `terminal/*` client methods inside the workspace only. t3code uses the same
  protocol for Cursor, Grok, and Antigravity (`reference/t3code/packages/effect-acp`,
  drivers under `apps/server/src/provider`). A new ACP harness needs only a
  runtime record (binary, digest, version, launch argv) and a capability row,
  not new adapter code.

## 5. Phases

| Phase | Delivers |
|---|---|
| 1 (this lane) | Extension registry (install from a pinned Git ref, validation, admin approval, immutable versions, "update available"), `extension` resource grants, Guild manifest, `extension@version/template` stage references frozen with digests, Claude Code embedded materialization (plugins, approved MCP/hooks), `AskUserQuestion` → `bx ask` mapping, Foundry phase/handoff signals → `bx event`, `guild` runtime image variant |
| 2 | `bx gate` and `bx artifact`, gate results in acceptance, review-page artifact rendering |
| 3 | `bx spawn`, recipe fan-out and conditional edges, decomposed Guild recipe |
| Alongside | Runner image pool and per-run selection; AX multi-image allowlist overlay; generic ACP adapter |

### Phase 1 limits

- Only Claude Code runs embedded extension stages. Other harnesses are
  refused at freeze.
- The AX runner allowlist is unchanged; extension stages need the `guild`
  image to be the configured runner image.
- Whether Claude Code's `--bare` mode executes plugin hooks is not yet proved
  by a live probe. Hooks are materialized only when approved, and the probe
  that confirms they run (or that `--bare` must be relaxed for approved hooks)
  is a Phase 1 follow-up.
- Serena is preinstalled in the `guild` layer, but Foundry's MCP server
  (`uv run foundry-mcp`) still resolves Python packages from PyPI at run
  time into each attempt's empty uv cache; the manifest declares that egress.
  The layer sets `UV_PYTHON_DOWNLOADS=never`, so tasks use only the
  preinstalled interpreter.
- The `guild` variant has passed `build.sh --check --variant guild` and a
  native arm64 smoke build of its layer (qemu cannot run uv for amd64 on the
  development Mac). The full amd64 image build and proof run on the AX
  development node.

## Appendix A: Guild manifest

For Guild at `5f147996c9ff80bebba9b9ab121a9c639a008d69` (forge 4.4.1, foundry
4.11.1), installed with an overlay manifest because the Guild repository does
not carry one. The file of record is
[`examples/extensions/guild/blaxsmith-extension.json`](../examples/extensions/guild/blaxsmith-extension.json);
this copy is for reading.

```json
{
  "$schema": "https://github.com/mjtechguy/blaxsmith/docs/extension-manifest.schema.json",
  "schema_version": "blaxsmith.extension/v1alpha1",
  "id": "guild",
  "version": "1.0.0",
  "publisher": {"name": "AlphaBravo", "url": "https://github.com/alphabravo-oss/guild"},
  "description": "Forge (specification interview) and Foundry (build-verify-fix loop) from the Guild Claude Code plugins, each run embedded in one stage.",
  "harnesses": [
    {"harness": "claude-code", "min_version": "2.1.229", "capabilities": ["plugins", "skills", "mcp", "subagents", "native_questions", "structured_protocol"]}
  ],
  "runtimes": [
    {"id": "uv", "version": "0.12.18"},
    {"id": "python", "version": "3.12"},
    {"id": "serena", "version": "1.7.0"}
  ],
  "egress": [
    {"host": "pypi.org", "port": 443, "purpose": "uv resolves foundry-mcp and serena-agent dependencies"},
    {"host": "files.pythonhosted.org", "port": 443, "purpose": "Python package downloads for foundry-mcp and Serena"},
    {"host": "github.com", "port": 443, "purpose": "Pinned extension checkout and Serena language-server downloads"},
    {"host": "objects.githubusercontent.com", "port": 443, "purpose": "Serena language-server release assets"},
    {"host": "registry.npmjs.org", "port": 443, "purpose": "Serena installs npm-based language servers on demand"}
  ],
  "native_subagents": true,
  "plugins": [
    {"id": "forge", "path": "plugins/forge"},
    {"id": "foundry", "path": "plugins/foundry"}
  ],
  "stage_templates": [
    {
      "id": "forge-plan",
      "title": "Forge specification interview",
      "mode": "embedded",
      "harness": "claude-code",
      "kinds": ["plan", "interview"],
      "plugins": ["forge"],
      "entrypoint": "/forge:plan",
      "interactions": ["forge-interview-round", "forge-mode-confirm"],
      "prompt": "You are running Guild Forge embedded in a Blaxsmith stage. Read {{plugin_root:forge}}/commands/plan.md and follow it exactly as the /forge:plan command, with the feature name and flags given in the stage prompt. Its bundled agents are installed. Every interview question goes through `bx ask` (kind interview_round for rounds, approval for the mode confirmation). Write the final spec where the command says, then give its path and requirement IDs in your final message."
    },
    {
      "id": "foundry-build",
      "title": "Foundry build-verify-fix loop",
      "mode": "embedded",
      "harness": "claude-code",
      "kinds": ["implement"],
      "plugins": ["foundry"],
      "entrypoint": "/foundry:start",
      "mcp_servers": ["foundry", "serena"],
      "hooks": ["foundry-serena"],
      "interactions": ["foundry-escalation"],
      "signals": [
        {"plugin": "foundry", "server": "foundry", "tool": "Foundry-Phase", "event": "phase", "name_field": "phase"},
        {"plugin": "foundry", "server": "foundry", "tool": "Foundry-Handoff", "event": "handoff", "name_field": "event", "text_field": "summary"}
      ],
      "prompt": "You are running Guild Foundry embedded in a Blaxsmith stage. Read {{plugin_root:foundry}}/commands/start.md and follow it exactly as the /foundry:start command with arguments `<SCOPE> --spec <frozen spec path> --no-ui`, using the frozen scope and specification named above. Do not run /foundry:setup or install launchd or systemd services; the sandbox already provides uv, Python 3.12, and Serena. Record every phase transition with Foundry-Phase and every handoff with Foundry-Handoff. Escalations go through `bx ask` (kind escalation). Between cycles run `bx inbox` and obey halt, pause, and set_max_cycles."
    }
  ],
  "mcp_servers": [
    {"id": "foundry", "plugin": "foundry", "server": "foundry", "transport": "stdio", "optional": false, "purpose": "Foundry state engine: phase gates, defect ledger, verification orchestration (uv run foundry-mcp)"},
    {"id": "serena", "transport": "http", "url": "http://127.0.0.1:9121/mcp", "optional": true, "purpose": "Serena LSP tools for TRACE and FLOW_TRACE; without it those streams report NOT_VERIFIED"}
  ],
  "hooks": [
    {"id": "foundry-serena", "plugin": "foundry", "event": "SessionStart", "command": "hooks/session-start-serena.sh", "optional": true, "purpose": "Starts the in-sandbox Serena daemon on 127.0.0.1:9121; never blocks the session"}
  ],
  "interactions": [
    {"id": "forge-interview-round", "kind": "interview_round", "title": "Forge interview round"},
    {"id": "forge-mode-confirm", "kind": "approval", "title": "Confirm brownfield, greenfield, or cosmetic mode"},
    {"id": "foundry-escalation", "kind": "escalation", "title": "Foundry escalation"}
  ],
  "checks": [
    {"id": "foundry-assay", "title": "Foundry assay verdict", "verdicts": ["pass", "fail"]}
  ],
  "ui": {
    "phases": [
      {"id": "start_cast", "title": "Cast (decompose)"},
      {"id": "cast", "title": "Cast"},
      {"id": "inspect_start", "title": "Inspect"},
      {"id": "inspect_clean", "title": "Inspect clean"},
      {"id": "grind_start", "title": "Grind"},
      {"id": "assay_fail", "title": "Assay failed"},
      {"id": "temper", "title": "Temper"},
      {"id": "nyquist", "title": "Nyquist audit"},
      {"id": "nyquist_done", "title": "Nyquist done"},
      {"id": "done", "title": "Done"},
      {"id": "halt", "title": "Halted"}
    ],
    "artifacts": [
      {"glob": "foundry-archive/*/defects.md", "renderer": "markdown"},
      {"glob": "foundry-archive/*/handoffs.md", "renderer": "markdown"},
      {"glob": "forge-specs/*.md", "renderer": "markdown"}
    ]
  }
}
```
