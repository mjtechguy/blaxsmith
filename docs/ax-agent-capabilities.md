# AX agent capability boundary

Pinned AX `TaskSpec` supplies an image, command, environment, workspace refs,
gateway, resources, and debug mode. It has no task field that carries skill
bytes, MCP configuration, or lifecycle hooks. `WorkspaceSpec` names MCP
servers and skill registries/path, but the pinned runner's `setupSkills` only
creates the path; it does not resolve registries, validate content, or wire
those servers into Codex, Claude Code, or OpenCode. Workspace planning can
mention MCP configuration, but does not make it a CLI capability.

Blaxsmith delivers declared `instructions`, applicable `AGENTS.md`, Forge
specification, decision transcript, and stage prompt as bounded instruction
text in the AX Task command. Selected `SKILL.md` files and explicitly declared
support files are digest-checked against the exact pinned checkout, validated
for portable Agent Skills metadata, and copied into the attempt's temporary
home for native discovery: Codex uses `$HOME/.agents/skills`, Claude Code uses
`--bare` plus a scoped `--add-dir` containing `.claude/skills`, and OpenCode
uses `$XDG_CONFIG_HOME/opencode/skills`. Only recipe-selected files are
copied. Harness-specific frontmatter that could grant tools, load hooks,
delegate subagents, or change model settings is rejected. Codex disables its
bundled unselected skills, native multi-agent, apps, plugins, and web-search features; OpenCode denies
its task, question, loop-recovery, web, and external-directory tools except
read-only access to selected skill files. Claude stays in bare mode, where
the scoped added directory is the only skill source. The worker rejects
ambient project CLI/MCP configuration and unlisted `AGENTS.md`. The command
contains no provider credential; a separate scoped bootstrap lease supplies
that at execution.

This implements content materialization and local adapter controls, not a live
proof that each pinned CLI discovers and loads the skill/support files or
rejects native delegation as configured. P1-28 stays blocked on those runtime
proofs and UI-visible provenance. AX creates the Task,
mounts the Workspace, and applies its Gateway; the runner's Workspace
`skills` field alone does not install skill content. MCP still needs a granted
server identity, endpoint/command policy, egress, secret binding, native CLI
configuration, and revocation proof. Hooks remain unsupported until they have
an explicit lifecycle and execution policy. The adapter stays pinned and
noninteractive, and launch stays disabled until end-to-end AX verification.
