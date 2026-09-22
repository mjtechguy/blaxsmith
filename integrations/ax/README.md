# AX runtime compatibility overlays

Upstream: `github.com/google/ax`, commit
`d8ed0fe38bceb7842d3c47817d53d16ccdfcb601`, Apache-2.0 (see LICENSE).
`fail-closed.patch`, `egress-policy.patch`, and `bootstrap-gate.patch` change the files named in their
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

## Rebuild and verify

Requires Go 1.27.1, Git, Python 3, and a clean checkout at the supported AX pin.
The output directory must be new. Binaries target Linux/AMD64; tests run on the
build host. The nil-task test needs a writable `/workspace` and skips otherwise;
it ran on the Linux development node.

```sh
bash integrations/ax/build.sh ../reference/ax /tmp/blaxsmith-ax-build
```

The script exports committed source into a temporary directory, applies all three
patches without changing the checkout, runs the full AX test suite and `go vet`,
and builds the controller and runner. It records source/patch/binary hashes in
`provenance.json`. Changing the upstream revision fails before building; update
the patch intentionally and repeat the runtime probes when adopting a new AX pin.
The [initial provenance](provenance.json), [egress follow-up
provenance](provenance-egress.json), and [bootstrap-gate provenance](provenance-bootstrap.json) are evidence of tested Linux builds, not
signatures.

## Synthetic bootstrap gate

When a Task supplies `BLAXSMITH_BOOTSTRAP_PUBLIC_KEY` as a base64 Ed25519 public
key, the runner serves a short-lived random challenge at
`/blaxsmith/bootstrap/challenge` and waits **before workspace setup and command
launch**. A release signed over the challenge, expiry, atespace, and task name
opens setup once. An empty, duplicate, or malformed configured key fails closed.
The challenge is generated on demand after activation; normal AX data-snapshot
resumes restore the pre-bootstrap golden runner and need a new release. The
actor template uses `/healthz` for Substrate transport readiness in this mode,
while AX keeps `/readyz` closed until workspace setup finishes.

The [live synthetic probe](../../docs/bootstrap-gate-probe.json) passed
blocked-before-release, release, and old-release rejection after suspend/resume
on gVisor. `deploy/dev/probe-bootstrap.py` reproduces it with an ephemeral test
signer. The first live attempt exposed a `/readyz` activation deadlock; another
showed one-worker golden-snapshot/cold-start contention. The probe now releases
the cold actor before waiting for the golden snapshot. Full AX tests and vet
passed on macOS and Linux after the readiness fix.

This gate is not authorization by itself: Task authors can choose a public key,
Substrate ingress is not client-authenticated, and the runner does not know its
actor UID or current execution owner. The future connector must inject and pin
the trusted signer key, verify activation-bound atunnel evidence and ownership,
then sign a release. No credential is accepted by this endpoint. Private Git
checkout, encrypted payload delivery, full-snapshot behavior, and revocation
remain open; do not use this slice for sensitive work.

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

This patch does not authenticate a worker, implement Connection → Grant → Binding
→ Lease, authorize startup, pin a private Git checkout, fence late results, or
revoke provider credentials. SystemInfo actor metadata is not a signed workload
credential. Readiness/initialization markers are not authority. The connector
still needs to verify the actual actor/template/image/pool and effective policy;
the overlay's template-name check alone does not verify their contents. The
native worker starts during template preparation too, so golden-snapshot creation
must be included in the future authenticated startup gate. Controller keys and
raw task environment fields remain separate concerns: users must not put secrets
in `spec.env`, task YAML, repository URLs or commands.

Phase 0 bootstrap/credential gates stay open. Only synthetic tasks run here.
