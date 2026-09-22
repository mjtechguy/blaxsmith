# Phase 0 implementation evidence

Inspected 2026-09-22. This is a development baseline, not a production support
matrix or a claim that Phase 0 is complete.

## Source inventory

| Input | Pinned commit | Use in this slice |
| --- | --- | --- |
| Guild | `dda615434dfb4624e1ab6328851afc91ef58e5e1` | Forge validator copied unchanged with MIT notice; original references remain outside this repository |
| AX | `d8ed0fe38bceb7842d3c47817d53d16ccdfcb601` | Execution/runner contract inspection; deployment target selected |
| Agent Substrate | `672533541dbfcd29084e4de2475267088bda3651` (revision required by AX's Go module) | Certificate APIs, gVisor worker, and snapshot persistence exercised |
| Astronomer | `5961992098c8d8c72a5159e966359ad61838579d` | Frontend source baseline; no frontend implemented yet |
| Coder | `c2f2c1708ed21119e5ccb639f4dfc20c9fb3b4d7` | Enterprise workspace reference; no code imported |

The local Go toolchain is 1.27.1; the compiler has no third-party Go dependencies.
Local Node is 25.8.0, outside Astronomer's inspected `>=24.21.0 <25` engine range.
Select a matching Node toolchain before frontend work; do not silently use the
current global Node. The local Rancher Desktop node is Linux/ARM64 on
`v1.35.7+k3s1`, below the inspected Substrate requirement of Kubernetes 1.36 with
the certificate APIs enabled. It is not the AX compatibility target.

The user supplied a dedicated remote Linux node for k3s/AX. Inspection found
Ubuntu 26.04.1, AMD64, 16 CPUs, roughly 30 GiB RAM and 575 GiB free disk, no
existing cluster, and no `/dev/kvm`. Use gVisor for the initial runtime proof;
microVM compatibility is unproven. Bootstrap uses pinned k3s `v1.36.4+k3s1`,
explicit certificate feature gates, a root-only kubeconfig, and SSH access.
Public ingress and ServiceLB are disabled. No model credentials are installed.

The remote k3s/Substrate/AX stack is now running. A synthetic AX command ran in
gVisor, guest access returned its output, and a separately written workspace
marker survived suspend/resume. The one-worker task was left suspended. The
[dev runbook](../deploy/dev/README.md) records the overlay, image build, access
rules, test procedure, and limitations; [image evidence](runtime-images.json)
records the deployed versions. This advances the AX runtime spike, not the
secure bootstrap/lease or tool-adapter acceptance gates.

## Guild adoption map

| Guild capability | Platform destination | Evidence / next work |
| --- | --- | --- |
| Forge interview transcript, locked/flexible requirements and citations | Frozen spec/transcript artifacts and evidence-linked platform Q&A | Original bytes and IDs retained; actual Forge validator runs; interactive Q&A still pending |
| Forge R4 validator | Trusted compilation gate | Embedded upstream script; passing v2.1 example; altered locked quote rejected |
| Foundry decomposition and frozen prompts | Recipe stages and per-task assignments | Named stages, dependencies, explicit profiles, frozen prompt hashes implemented; task decomposition and full prompt-pack import pending |
| Foundry dispatch and correction loop | Platform controller using AX adapters | Do not import Claude Agent/TeamCreate calls, tmux coordination, singleton active-run state, or local file locks as distributed orchestration; no controller yet |
| Holmes/TLDR research and evidence gathering | Optional `research` stages | Stage shape supported; tool integration and evaluation pending |
| Crucible/E2E verification | `verify` stages plus human-owned frozen check policy | Required check names and gate ordering validated; trusted check runner pending |
| UX review | Optional `ui_review` stages | Composition shape only; browser/tool adapter and acceptance evidence pending |
| Webster documentation | Optional `documentation` stages | Composition shape only; recipe can include the specialist before final review |

Remaining Guild plugins and their native invocation/installation behavior are
deferred until individually mapped and tested. New example prompts are not a
claim that the complete Guild prompt system has been ported. Reuse its meaningful
artifacts and validators, not its local process topology.

## Completed checks and remaining gates

`make check` runs real temporary-Git integration tests and `go vet`. Tests cover
stable frozen bytes despite dirty files and moved refs; per-artifact and bundle
digests; scoped `AGENTS.md` and skill inclusion; graph cycles and missing gates;
agent impersonation of human approval; absent profiles; finite limits; duplicate
JSON; invalid paths; symlinks; LFS pointers; oversized inputs; and Guild fidelity
failure. Tests do not launch agents or require reference clones.

Remaining P0 evidence includes approved platform contracts, provider/version
capability tests, AX/Substrate sandbox execution and failure recovery, secure
bootstrap and revocation, actual Guild ticket baselines, Astronomer adoption,
and the technology-guide snapshot. Broad P0 tasks remain unchecked.

AX observations that must inform the connector: the controller always invokes
`/usr/local/bin/ax-task-runner`; the default runner logs workspace setup errors
but still starts the command; AX does not collect the command exit result; the
runner stays alive after command exit; resume restores files with a new process
tree. The bridge needs explicit preconditions and durable result reporting.
`AX_TASK_YAML` and metadata can expose raw task environment fields, so credentials
must not be placed there. These are inspected behaviors, not a completed security
test or a validated worker contract.

Additional inspected blockers: AX currently falls back to the default template
when creating a task-specific template fails, and continues after an egress
policy application error. The connector must verify the exact installed template
and effective policy before releasing a lease. Substrate's pinned authentication
guide explicitly says authorization/RBAC enforcement is not implemented; defining
an authorization model in source does not prove tenant isolation. Keep this dev
cluster restricted to synthetic work until the platform and runtime boundary
tests pass. The AX upstream example runner image also returned HTTP 403 to an
anonymous pull; the dev smoke runner is built from the pinned AX source instead.

Observed runtime retry gap: initial submission failed with `ResourceExhausted`
while Substrate created the golden snapshot. AX did not automatically retry once
the worker became free. An explicit resume succeeded and the persistence checks
then passed. Distinguish transient capacity/template preparation from genuine
task failure; add bounded, visible retries to the connector rather than a model
correction loop. Public IPv4 access to the control-plane/runtime ports was blocked
in probes; IPv6 reachability testing was unavailable from the local network.
