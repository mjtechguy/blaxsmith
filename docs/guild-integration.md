# Optional Guild integration

Guild is an optional factory. Blaxsmith's neutral recipe, workflow, dispatch, API and MCP contracts do not require it; Anvil remains the shipped default.

The trusted Forge validator is pinned to Guild commit `dda615434dfb4624e1ab6328851afc91ef58e5e1` in `internal/guild/validate.go`. Validation records the validator ID, source revision and embedded script digest. This pin qualifies the selected specification validator; it does not qualify every Guild plugin or live Foundry execution.

| Guild behavior | Blaxsmith mapping | Evidence / remaining work |
| --- | --- | --- |
| Forge specification and transcript | Explicit `guild-forge` validator with named frozen inputs | Local validator/recipe checks. Arbitrary repository validators are never executed by the control plane. |
| Forge / Foundry plugin content | [Extension manifest](../examples/extensions/guild/blaxsmith-extension.json), pinned installation and approved permissions | Content frozen by digest. Live pinned plugin/harness qualification remains outstanding. |
| Interview questions | `bx ask` with a stable interaction ID | Questions become durable platform interactions. Native interactive widgets are inappropriate for unattended runs. |
| Foundry phases and handoffs | Declared MCP tool signals become `attempt.progress` events | Progress is advisory; it is not acceptance evidence. |
| Findings, artifacts and checks | Existing `bx artifact` / `bx gate` evidence bridge | Evidence must identify the current candidate and conform to frozen check policy. Guild ledger assertions alone do not satisfy required checks. |
| Local subagents | Explicitly approved native subagents inside one attempt | They share the sandbox. Independent platform children, delegated credentials and child budgets remain unimplemented. |
| Repair loops | Bounded embedded work or shared recipe correction policy | Select one owner per loop. Do not run an unbounded Foundry loop inside an independently continuing platform loop. |
| Pause/cancel | `bx inbox` steering plus platform runtime stop | Guild must obey instructions and stop spawning local work. Only confirmed runtime termination proves cancellation. |
| Optional Serena | Approved sandbox-local MCP/hook | Missing capability is `NOT_VERIFIED`; it cannot become a passing validation result. |

The extension manifest currently requests Claude Code `2.1.229`, uv `0.12.18`, Python `3.12`, Serena `1.7.0`, declared egress, selected hooks/MCP servers and native subagents. These are manifest requirements, not a live compatibility certification. Hostnames listed as egress requirements do not prove that the installed AX/Substrate network policy can enforce them; preflight and live networking qualification must establish that.

For an external Guild coordinator, follow the [shared factory quickstart](factory-integration.md): use project-scoped platform credentials, save Guild-owned opaque plans, and bind a neutral recipe to the exact goal/plan revision. Provider credentials stay behind platform grants and bootstrap. A Guild coordinator cannot mint child credentials, increase goal allowances or claim human review authority from its own configuration.

Before enabling a new Guild version, compare the pinned commands, manifests, tool signal names, question shapes, cancellation behavior and evidence format. Run local recipe/adapter checks, then the live interview/build/review/cancellation matrix on an isolated configured runtime. Record the exact Guild commit, extension digest, approved permissions, harness image, AX pin and policy used. No managed delegation or cross-factory acceptance qualification is claimed today.
