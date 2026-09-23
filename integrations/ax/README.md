# AX runtime compatibility overlays

Upstream: `github.com/google/ax`, commit
`d8ed0fe38bceb7842d3c47817d53d16ccdfcb601`, Apache-2.0 (see LICENSE).
`fail-closed.patch`, `egress-policy.patch`, `bootstrap-gate.patch`,
`platform-bootstrap-key.patch`, `encrypted-git-bootstrap.patch`,
`command-exit-readback.patch`, `task-tombstones.patch`, `redis-ha.patch`, and
`consumer-recovery.patch` change the files named in their
diffs; the reference checkout stays untouched. This is a temporary integration overlay, not a claim that AX
has accepted these changes or that secure bootstrap is finished.

The patch closes observed launch failures at their source:

- Missing referenced gateways/workspaces stop reconciliation before actor creation.
- Template errors or an unexpected template identity cannot select the default
  template. Policy-application errors cannot proceed to ResumeActor. Both paths
  also attempt to suspend any previous execution; a failed stop remains visible.
- Suspend does not depend on a new template or policy being provisioned.
- Controller environment/Kubernetes Gemini keys are no longer copied into actor
  templates. The dev controller's now-unused secret RoleBinding was removed.
- Empty allowlists are sent to the policy API instead of silently ignored.
- Missing/duplicate workspace definitions, failed Git checkout, missing required
  bootstrap, failed skills/state setup, and marker-write errors stop the runner
  before command launch. A Git failure also prevents bootstrap execution.
- Resume probes the current runner instead of trusting an old Ready condition.
- The egress follow-up rejects hostname and port rules the pinned Substrate
  gateway cannot enforce, keeps `0.0.0.0/0` as IPv4 CIDR rather than treating it
  as all destinations, defaults tasks without a gateway to deny-all, and updates
  an existing policy to empty deny-all.
- Task templates now contain stable launch inputs, excluding status, creation
  time, and the suspend flag from their hash. This stops each resume from
  creating another template and racing its golden snapshot. AX retries only
  transient capacity or golden-snapshot readiness failures, at most four resume
  calls, and attempts to stop the actor when resume ultimately fails.
- [Task-name tombstones](../../docs/ax-task-tombstones.md) stop a timed-out
  `UpdateTask` from recreating a deleted attempt, including when `DeleteTask`
  reaches AX before the first upsert.
- [Redis connection overlay](../../docs/ax-redis-ha.md) adds authenticated TLS
  and Sentinel master discovery to both AX API and controller processes.
- [Consumer recovery overlay](../../docs/ax-redis-ha.md) reclaims stale pending
  stream entries, renews active ownership, and rejects former-owner acknowledgements.

## Rebuild and verify

Requires Go 1.27.1, Git, Python 3, and a clean checkout at the supported AX pin.
The output directory must be new. Binaries target Linux/AMD64; tests run on the
build host. The nil-task test needs a writable `/workspace` and skips otherwise;
it ran on the Linux development node.

```sh
bash integrations/ax/build.sh ../reference/ax /tmp/blaxsmith-ax-build
```

The script exports committed source into a temporary directory, applies all nine
patches without changing the checkout, runs the full AX test suite and `go vet`,
and builds the server, controller and runner. It records source/patch/binary hashes in
`provenance.json`. Changing the upstream revision fails before building; update
the patch intentionally and repeat the runtime probes when adopting a new AX pin.
The [initial provenance](provenance.json), [egress follow-up
provenance](provenance-egress.json), [bootstrap-gate provenance](provenance-bootstrap.json),
[platform-key provenance](provenance-platform-key.json), [encrypted-Git
provenance](provenance-encrypted-git.json), and [task-tombstone
provenance](provenance-task-tombstones.json), and [Redis connection
provenance](provenance-redis-ha.json), and [consumer-recovery
provenance](provenance-consumer-recovery.json) are evidence of tested Linux builds, not
signatures.

## Synthetic bootstrap gate

When the controller supplies `BLAXSMITH_BOOTSTRAP_PUBLIC_KEY` as a base64 Ed25519
public key, the runner serves a short-lived random challenge at
`/blaxsmith/bootstrap/challenge` and waits **before workspace setup and command
launch**. A release signed over the challenge, expiry, atespace, and task name
opens setup once. An invalid configured key or any Task-level override fails closed;
the Blaxsmith deployment must enable the controller flag for every Task.
The challenge is generated on demand after activation; normal AX data-snapshot
resumes restore the pre-bootstrap golden runner and need a new release. The
actor template uses `/healthz` for Substrate transport readiness in this mode,
while AX keeps `/readyz` closed until workspace setup finishes.

The [original live synthetic probe](../../docs/bootstrap-gate-probe.json) passed
blocked-before-release, release, and old-release rejection after suspend/resume
on gVisor. The first live attempt exposed a `/readyz` activation deadlock; another
showed one-worker golden-snapshot/cold-start contention. The probe now releases
the cold actor before waiting for the golden snapshot. Full AX tests and vet
passed on macOS and Linux after the readiness fix.

The [platform-key follow-up](platform-bootstrap-key.patch) makes the AX
controller inject a configured public key into every Task template, rejects
Task-supplied signer keys, and admits only the configured digest-pinned runner
image while gating is enabled. The runner rejects a signer in Task YAML and
reads only its injected process environment. The [live probe](../../docs/bootstrap-platform-key-probe.json)
blocked a forged signer and another image before actor launch, then passed
release and replay checks using a root-owned test signer outside the repo.
The [synthetic ledger-backed connector](../../docs/bootstrap-ledger.md) now
verifies the activation-bound actor proof and current owner/runtime before
signing. Its authorizer still has no product grant or measured egress decision.
The [encrypted-Git overlay](encrypted-git-bootstrap.patch) now accepts a
synthetic setup credential only inside an envelope for the challenge's fresh
guest X25519 key. Its signature covers the ciphertext hash. The runner uses
Git askpass only during the exact HTTPS fetch, verifies the fetched commit
against the encrypted release before checkout, clears the credential before
starting the command, and rejects a pre-existing `.git` without the
workspace-volume marker or with a changed remote or expected commit. The pinned runner image
uses `alpine/git@sha256:8c843da8f112867e5d713f3bce85fbe815ec5582bd76bddb2e9121f8c7af9e8f`;
the synthetic private CA was included in the exact [tested image](../../docs/bootstrap-private-git-probe.json).
The [live private-Git probe](../../docs/bootstrap-private-git-probe.json) and
[bounded secret scan](../../docs/bootstrap-private-git-secret-scan.json) pass.
Product grants, complete egress, full-snapshot behavior, and revocation remain
open; do not use this slice for sensitive work.

The initial launch regression tests were also run against unmodified upstream
production code. They failed on template fallback, policy failure, hidden stop failure,
ambient credentials, missing inputs, ignored empty allowlists, stale resume
readiness, and incomplete workspace setup. The follow-up tests cover policy
translation, deny-by-default, stable template names, and bounded resume retry.
They pass with the patches. Existing
positive command, exit-code, process shutdown, workspace and controller tests
also pass. No model or production credential is needed for these tests.

## Runtime evidence and limits

[The live probe report](../../docs/launch-probe.json) records successful gVisor
command execution, five blocked runner cases, missing-input rejection before
actor creation, and file persistence after suspend/resume. Run it using
`deploy/dev/probe-launch.py` on the prepared dedicated node. The developer
runbook describes publishing and deploying the verified binaries.

The first [live probe](../../docs/launch-probe.json) exposed missing dataplane
enforcement: an empty policy still reached the registry. The follow-up
[Substrate patch](../substrate/README.md) closes that CONNECT path; the new
[probe](../../docs/egress-probe.json) passes allow-all, empty, matching-CIDR,
nonmatching-CIDR and missing-gateway cases against one reachable endpoint. `GatewayReady /
PoliciesApplied` still only reports successful storage, not a measured dataplane
check. Complete network bypass, existing-tunnel revocation, and credential
boundaries remain open before sensitive or untrusted work.

The combined overlays and connector now authenticate the synthetic worker and
fence a private checkout to the supplied commit. They do not implement product Connection → Grant →
Binding → Lease authority, bind that commit to an authorized frozen bundle, fence late results,
or revoke provider credentials. Readiness/initialization markers are not
authority. The product connector still needs current policy and effective
egress verification; the dev callback checks only image/pool/gVisor. The
native worker starts during template preparation too, so golden-snapshot creation
must be included in the future authenticated startup gate. Controller keys and
raw task environment fields remain separate concerns: users must not put secrets
in `spec.env`, task YAML, repository URLs or commands.

Phase 0 bootstrap/credential gates stay open. Only synthetic tasks run here.

## Command exit readback

The pinned gated runner now serves `GET /blaxsmith/command-exit`. It returns
HTTP 202 before the child command exits and a small JSON record after `Wait`:
activation nonce, AX atespace/task, SHA-256 of JSON-encoded `spec.command`, exit
code, signal, interrupted flag, sequence 1, and the one-time observation time.
The stable time makes an exact signed receipt replayable after an uncertain
connector response. It is absent without the
platform bootstrap gate. A setup or command-start error produces no exit
record. The runner stays available for readback after the child exits.

The product connector must read through the authenticated route for the
*current* actor, verify the active owner, actor UID, template, image, command
hash, and bootstrap activation, then sign an attempt-bound
`internal/runnerexit.ExitReport` with a key held outside the guest. The signed
report records that observation; it does not assert task success or verify
artifacts. The platform must not finish an attempt from exit code alone.

The runner and child currently share a container security boundary. In
particular, hostile same-UID code may tamper with the runner process or its
readback. This is suitable only for the synthetic integration path until
process isolation or an external trusted supervisor is proved on the node.
