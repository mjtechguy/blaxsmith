# AX inputs for a public-source tool attempt

The pinned AX `TaskSpec` supports `workspaces[]` and `gateway`. The patched
controller rejects a missing reference and applies the referenced Gateway's
egress policy before resuming the actor. Blaxsmith binds an operator-selected,
empty Workspace at `/workspace`; its worker fetches and checks out the frozen
public Git commit after bootstrap. The empty AX Workspace avoids an unpinned
second Git checkout, MCP server, skill mount, or bootstrap goal.

Before reservation, `Bridge.CheckToolInputs` reads both resources in the
organization's `blaxsmith-<uuid-without-hyphens>` atespace. The Gateway must
have no listeners and exactly the current public IPv4 `/32` DNS answers for
the Git host and `api.openai.com` or `api.anthropic.com`. Wildcards, broad
CIDRs, private addresses, ports, extra destinations, missing resources, and
unavailable DNS block dispatch. The Task carries the validated resource names,
and AX resolves them again when reconciling the Task.

This is configuration validation, not a measured network proof. The pinned
Substrate gateway enforces IP/CIDR rules rather than DNS names or TLS SNI;
shared IPs, DNS changes, existing tunnels, alternate paths, and resource
updates after preflight need live-node testing and stronger controls. A changed
DNS answer will normally fail closed at runtime. `PreflightWorker` remains a
separate mandatory admission gate for the approved image/pool and measured
route. Keep launch readiness disabled until the combined runner image, model
credential delivery, these AX resources, and the live egress probe are wired.
