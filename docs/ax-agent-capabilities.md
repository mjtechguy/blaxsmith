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

| Capability | AX/Substrate provides | Blaxsmith must provide |
|---|---|---|
| Sandboxed execution | AX Task reconciliation and actor lifecycle; Substrate gVisor, placement, and observed worker runtime | Approved image/model/resource selection, policy checks, durable attempt ownership, credentials, result acceptance, and human review |
| Workspaces | AX Workspace definitions, Git setup, mounted paths, and readiness reporting | Frozen repository/commit, per-attempt names and writable isolation, private-Git grant/lease, verified checkout, instruction materialization, and attributable changes |
| Networking | AX Gateway attachment and Substrate policy application for supported flows | Destination authorization, DNS/redirect resolution, narrow CIDR policy, effective-route measurement, policy-change fencing, and revocation evidence |
| MCP servers | Workspace schema fields for declaring servers | Server identity and grants, endpoint/egress policy, scoped credentials, each harness's native configuration, call auditing, and revocation |
| Skills and instructions | Workspace skill-path setup; it does not resolve selected content or configure the coding CLIs | Resolve and hash approved `SKILL.md` support files and scoped `AGENTS.md`, freeze provenance, materialize only selected inputs into each CLI's discovery path, disable ambient/delegated tools, and show effective inputs in the UI |
| Agent lifecycle | AX Task/actor state, suspend/resume, worker assignment, and startup/setup status | Workflow stages, fair dispatch, owner fences, connection leases, steering/stop, trusted exit/readback, artifacts/checks, architect validation, audit, and final human approval |
| Human workspace | No product UI or user authorization model | Tenant/RBAC-aware workspace, durable Q&A and activity, SSE replay, review, configuration, audit, and operational controls |

Run details now expose the frozen harness/model/effort and selected instruction
and skill file digests from the validated recipe bundle. This is configured-input
provenance; it does not yet prove a native CLI loaded the files.

The credential-free [native skill probe](native-skill-probe.json) now proves
that the pinned Codex, Claude Code, and OpenCode CLIs each discover and load the
selected skill body with the same SHA-256 across two fresh launches. It uses a
loopback mock model endpoint, a fake key, and a container with external
networking disabled. It does not prove real-provider authentication, every
permission denial against hostile requests, MCP calls or revocation, or AX
workspace/lifecycle behavior.

After bootstrap activation, dispatch now waits for AX's exact
`WorkspaceReady=True/SetupComplete` condition and rechecks the actor against
the frozen runtime binding before reporting `started`. This closes the
premature-status path; the live attempt-input probe deliberately stayed behind
the bootstrap gate, so the authenticated startup/readiness transition still
needs a live proof.

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
bundled unselected skills, native multi-agent, apps, plugins, and web-search
features. OpenCode v2 denies subagents, questions, web fetch/search, Code Mode
`execute`, unselected skills, and external-directory access. In-workspace shell
execution remains available for coding inside AX's sandbox and network policy.
Final `.env` denials follow selected-skill read grants because OpenCode v2
[uses the last matching permission rule](https://opencode.ai/v2/docs/permissions).
Claude stays in bare mode, where
the scoped added directory is the only skill source. The worker rejects
ambient project CLI/MCP configuration and unlisted `AGENTS.md`. The command
contains no provider credential; a separate scoped bootstrap lease supplies
that at execution.

This implements resource forwarding, content materialization, and local
adapter controls. The native probe establishes selected-skill discovery/load
for the pinned versions, while P1-28 remains open for MCP transport/grants,
allowed and denied MCP calls, server revocation, and UI-visible effective
permission sources. It also does not prove every native delegation or tool
denial against hostile requests. AX creates the Task,
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
dataplane. The [live attempt-input probe](ax-attempt-inputs-probe.json) verified
the Workspace/Gateway/Task bindings, observed the Substrate runtime, and proved
actor-gone cleanup with no credential release. It did not prove Workspace
setup completion, checkout isolation, effective route enforcement, or revocation
convergence. P1-26/27 remain open for those runtime proofs and the policy-derived
destination contract.

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
