# AX agent capability boundary

Pinned AX `TaskSpec` supplies an image, command, environment, workspace refs,
gateway, resources, and debug mode. It has no task field that carries skill
bytes, MCP configuration, or lifecycle hooks. `WorkspaceSpec` names MCP
servers and skill registries/path, but the pinned runner's `setupSkills` only
creates the path; it does not resolve registries, validate content, or wire
those servers into Codex, Claude Code, or OpenCode. Workspace planning can
mention MCP configuration, but does not make it a CLI capability.

Blaxsmith now delivers the recipe's declared `instructions` and `skills`
alongside applicable `AGENTS.md`, Forge spec, decision transcript, and stage
prompt as **instruction text** in the AX Task command, capped below Linux's
single-argument limit. Every selected
file comes from the run's immutable bundle. Dispatch verifies its SHA-256,
and the worker verifies the same path and digest against its exact pinned Git
checkout before invoking the CLI. Missing, changed, duplicate, oversized, or
unlisted declared files block the attempt. The worker also rejects unlisted
`AGENTS.md` and project CLI configuration in the checkout, including nested
configuration that the agent might discover while navigating the tree. The
command contains no provider
credential; a separate scoped bootstrap lease supplies that at execution.

This is prompt context, not native skill installation or executable skill
support. The current recipe has no active MCP or hook declarations; neither
AX Workspace MCP fields nor project-local CLI settings are translated into
them. Native skills need a separately approved installation and discovery
contract with a content digest and per-tool behavior. MCP needs a granted
server identity, endpoint/command policy, egress, secret binding, and
revocation; hooks need an explicit lifecycle and execution policy. Until
those contracts exist, the tool adapter stays on its current pinned,
noninteractive CLI path.
