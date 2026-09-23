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

The follow-up [AX patch](../integrations/ax/README.md) is deployed as a separate
controller image and synthetic runner image. Reference checkouts are unchanged.
It closes the launch failures listed below, removes ambient controller-key
injection, and rechecks resumed runner readiness. The full patched AX test suite
and vet pass on macOS and Linux (the root-workspace test runs only on Linux here).
New regression checks fail against unmodified upstream code. The
[live report](launch-probe.json) records blocked missing inputs, five runner
failure cases, valid command execution, and suspend/resume file persistence.

The first empty-egress-policy test **failed enforcement**: the pinned gateway
authenticated actor identity but ignored stored destination rules. A separate
[Substrate overlay](../integrations/substrate/README.md) now checks policy on
new CONNECTs, and the [AX follow-up](../integrations/ax/README.md) rejects
unsupported hostname/port rules, defaults missing gateways to deny-all, and
updates empty policies. The new
[gVisor probe](egress-probe.json) passed allow-all, empty, matching-CIDR and
nonmatching-CIDR cases against the same registry endpoint, plus deny-by-default
without a gateway. This is one tested
dataplane path, not a complete egress or revocation boundary. No worker identity,
bootstrap replay protection, credential custody/lease lifecycle, private checkout,
or real model adapter has been implemented in this slice. P0-04/05 remain open.

The follow-up probe also exposed an AX resume race: its task-template hash
included mutable status and the suspend flag, generating a new template during
resume before its golden snapshot was usable. The AX overlay now hashes stable
launch inputs and uses a bounded retry for the observed transient snapshot and
capacity errors. A later [probe](egress-probe.json) passed suspend/resume and
the egress cases together. This does not prove arbitrary restart or snapshot
recovery. The full Substrate `make verify` attempt on a source export did not
pass: an unrelated control-API integration test timed out at 10 minutes and an
envtest check required a Git checkout. The changed `atenet` package tests and
vet passed on macOS and the Linux node.

The [worker-identity spike](bootstrap-identity-spike.md) found no projected
pod-identity certificate or service-account token in the tested AX/gVisor
guest. Substrate keeps the actor certificate/key with `atunnel`; task metadata
and AX guest access are not authenticated bootstrap proofs. P0-04 now targets
an activation-bound attestation and pre-workspace gate, with synthetic replay,
snapshot-clone, and actor-replacement checks before any credential delivery.

The [synthetic bootstrap-gate probe](bootstrap-gate-probe.json) and
[Linux build provenance](../integrations/ax/provenance-bootstrap.json) now show a
signed pre-workspace release and fresh challenge after data-snapshot resume.
The first live run exposed that Substrate's `/readyz` probe deadlocks before
bootstrap; the patch gives gated templates a separate `/healthz` transport
probe while AX retains `/readyz` for workspace readiness. Golden-template
warmup can also contend with the task on a one-worker pool. At that point,
actor UID proof,
connector/client authentication, credential delivery, private Git, and
full-snapshot/revocation behavior remain open. P0-04 is not complete.

The next [Substrate overlay](../integrations/substrate/README.md) uses its
activation-specific `atunnel` key to sign the guest challenge and a fresh
connector nonce. The [live actor proof](actor-attestation-probe.json) passed
cluster-CA/current-actor-UID verification and rejected a wrong UID, nonce,
body, CA, malformed nonce, and wrong route. The key remains outside gVisor;
the task ended suspended. The source and Linux build are pinned in
[attestation provenance](../integrations/substrate/provenance-attestation.json).
This adds evidence for worker identity. The next
[router-auth probe](bootstrap-router-auth-probe.json) uses verified HTTPS,
TokenReview, exact service-account identity, and an audience-scoped token for
bootstrap routes. Denied requests did not resume the actor; the valid request
still passed actor proof verification. The router strips the token before
forwarding. The [PostgreSQL challenge ledger](bootstrap-ledger.md) now stores
only a nonce hash and consumes an owner/actor-bound proof once; a local real
database test covers wrong scope, replay, concurrency, expiration, and owner
or actor replacement. It is not yet connected to the scheduler or connector.
There is still no live owner/actor/policy recheck at release, credential
delivery, or private checkout, so P0-04/05 remain open.

The next pinned [Substrate actor-UID patch](../integrations/substrate/README.md)
compares the connector's expected UID after router resume and again at the
worker's current `atunnel` activation. The [live proof](bootstrap-actor-fence-proof.json)
and [gate probe](bootstrap-actor-fence-gate.json) rejected a stale UID while
preserving valid proof and replay behavior. The ledger now provides
compare-and-swap `Assign`/`Deactivate` owner transitions; real scheduler and
connector wiring, authorization, and access delivery remain open.

The [AX platform-key patch](../integrations/ax/README.md) now takes the
bootstrap public key from the controller, gates every task on the configured
digest-pinned runner, and rejects a Task-supplied signer. The
[live probe](bootstrap-platform-key-probe.json) blocked a forged signer and
unapproved image before actor launch, then passed release and replay checks
with a root-owned synthetic signer outside the repo. No product connector or
private Git access is involved yet. The [synthetic ledger connector
probe](bootstrap-ledger-release-probe.json) then used the PostgreSQL owner and
challenge ledger, authenticated router, current AX/Substrate runtime check,
and controller-owned signer to open the same actor before and after a data
snapshot resume. It recorded release intent/completion twice, rejected replay,
and deactivated the owner after suspension. Its authorization check is still
synthetic; effective egress, product grants, encrypted credential delivery,
private checkout, full-snapshot behavior, and recovery remain open.

The [private-Git probe](bootstrap-private-git-probe.json) then delivered a
synthetic token in an X25519/AES-GCM envelope bound to the actor-signed guest
key and the one-use release. A pinned Git-capable runner fetched an authenticated
HTTPS fixture before the task command. A missing `/ax` marker on data-snapshot
resume initially blocked the checkout; the runner now places a URL-and-commit-bound marker
on the snapshotted workspace volume and rejects an existing `.git` without it.
The connector now reads the live template's pause/commit/resume snapshot
settings before each release and denies settings other than data-only pause
and commit with golden-image resume. The dev policy pins the snapshot bucket.
The [rerun](bootstrap-private-git-probe.json) observed data-only external
snapshots after both suspensions and passed private checkout and replay checks.
The encrypted Git setup now carries an expected commit; the runner checks the
fetched revision before checkout. A wrong revision failed the focused AX test,
and the [live rerun](bootstrap-private-git-probe.json) checked the fixture's
exact SHA before command execution and again after data-snapshot resume.
The resumed task required a new proof and kept the private checkout. The
[latest synthetic rerun](bootstrap-private-git-probe.json) seeded an exact
provider/connection/project/grant/binding chain and stored the fixture token
as encrypted database ciphertext. The connector checked current rows and read
the secret under the release transaction for both initial setup and resume.
Each release first reserved a distinct access lease with the actor UID and
owner generation; [the live report](bootstrap-private-git-probe.json) confirms
both were marked delivered with secret version 1. These are platform delivery
records, not provider-enforced expiration of the raw fixture token. The
[surface scan](bootstrap-private-git-secret-scan.json) found no token in the
listed persisted/logged surfaces. This does not prove full-snapshot memory
exclusion, effective egress on every path, authenticated grant/revocation, or trusted
binding from a frozen input bundle to that SHA; P0-04/05 remain open.

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
must not be placed there. The launch overlay blocks observed setup failures, but
these are not a completed security test or a validated worker contract.

Additional inspected upstream blockers: unpatched AX falls back to the default template
when creating a task-specific template fails, and continues after an egress
policy application error. The deployed patch removes both fallbacks. The connector
must still verify the exact installed template
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
