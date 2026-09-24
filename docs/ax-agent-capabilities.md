# AX agent capability boundary

Pinned AX `TaskSpec` supplies an image, command, environment, workspace refs,
gateway, resources, and debug mode. The upstream controller at this pin did not
forward resource requests or limits to Substrate. Blaxsmith's
[`task-resources.patch`](../integrations/ax/task-resources.patch) maps them to
the Substrate actor template and guest OCI spec. Substrate has one actor-level
CPU/memory bound used for both placement accounting and gVisor sandbox sizing,
so the AX controller rejects unequal requests and limits at this pin. The
initial tool task uses equal 1 CPU/1 GiB bounds. The [live probe](ax-resource-limits-probe.json)
observed those template values and the corresponding `_pause` host cgroup caps
with Task debug off. This is a single-node configuration proof; independent
request/limit values and admin-managed resource classes remain future work. AX
has no task field that carries skill
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

This implements resource forwarding, content materialization, and local adapter controls, not a live
proof that each pinned CLI discovers and loads the skill/support files or
rejects native delegation as configured. P1-28 stays blocked on those runtime
proofs and UI-visible provenance. AX creates the Task,
mounts the Workspace, and applies its Gateway; the runner's Workspace
`skills` field alone does not install skill content. MCP still needs a granted
server identity, endpoint/command policy, egress, secret binding, native CLI
configuration, and revocation proof. Hooks remain unsupported until they have
an explicit lifecycle and execution policy. The adapter stays pinned and
noninteractive, and launch stays disabled until end-to-end AX verification.

For public Git runs, the dispatcher creates an attempt-named AX Workspace with
the frozen repository and commit object ID as its fetch ref, in a `source`
child directory. The AX Task binds that definition at `/workspace`; the worker
rejects path traversal/symlinks and verifies the child's origin URL and `HEAD`
against the frozen repository and commit before launching a CLI. This avoids
fetching a moved branch and then discovering the mismatch in the worker. The
bridge reads the exact Workspace back before releasing the model lease and
removes it after stop proves the actor is gone. Private-repository binding
selection from the product is still missing; the encrypted AX bootstrap
capability is not yet exposed by project settings or dispatch.

The dispatcher also copies the validated public-Git/model CIDR allowlist into
an attempt-named Gateway before it creates the Task. The Task references this
per-attempt Gateway, and the bridge checks its readback before launch and again
before credential release. Both attempt resources are removed after actor
absence is proved. This narrows shared-name races; AX does not make the
definitions immutable, and these reads do not prove the effective network
dataplane. P1-27 still needs live route and revocation evidence.

## Substrate connector authority

At the pinned Substrate revision, `kubectl-ate --endpoint` still loads
kubeconfig to read the ate-api ClusterTrustBundle. The reviewed
[`direct-ate-ca.patch`](../integrations/substrate/direct-ate-ca.patch) adds a
direct mode that uses a mounted CA file and explicit token file without
Kubernetes API permissions. The token authenticates the connector, but the
pinned ate-api actor methods do not enforce fine-grained caller authorization;
the connector therefore remains trusted for the whole registered AX control
plane. Blaxsmith must authorize every assignment, keep the AX API private to
the connector, and preserve that cluster as a trust boundary until the AX API
provides enforced scoping. This connector identity does not replace a user or
provider Connection, Grant, Binding, or Access Lease. The [pinned Substrate
authentication contract](https://github.com/agent-substrate/substrate/blob/672533541dbfcd29084e4de2475267088bda3651/docs/authentication.md)
states the current whole-control-plane authorization limit.

The AX overlay build now also compiles and records the pinned `ax` CLI for the
future connector image. Its non-loopback, plaintext API constraint means the
connector still needs an explicitly managed local tunnel to a private AX API;
it must never fall back to a workstation kubeconfig. This CLI packaging check
does not yet provide the connector deployment or the tunnel itself.
