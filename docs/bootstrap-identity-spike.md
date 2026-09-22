# AX/Substrate worker identity spike

Inspected 2026-09-22 against AX `d8ed0fe38bceb7842d3c47817d53d16ccdfcb601`
and Substrate `672533541dbfcd29084e4de2475267088bda3651`, including the
synthetic gVisor task in `blaxsmith-launch-95350846d8`. This records a blocked
security contract, not a credential-delivery implementation.

## What the current runtime proves

- Substrate's `atunnel` generates and retains the actor's private key outside
  the guest. Its short-lived certificate is scoped to the actor UID and to the
  `atunnel` purpose (`internal/atunnel/credential.go`,
  `cmd/ateapi/internal/controlapi/actor.go`). The AX task runner does not receive
  that certificate or key.
- Inside the running gVisor guest, file-existence checks found no
  `/run/podidentity.podcert.ate.dev/credential-bundle.pem`, no Kubernetes
  service-account token, and no `/run/actor-id-ca-certs/ca.crt`. No credential
  contents were read. This is one image/worker-path observation, not a proof
  that all future runtime variants lack projected identity.
- AX's `AX_TASK_YAML` and localhost metadata service describe a task but are
  supplied by the controller/guest. Neither proves to an external broker which
  actor UID is running. The task may start during golden-template preparation,
  before there is a final actor/attempt binding (`internal/controller/reconciler.go`,
  `runner/runner.go`).
- The Substrate ingress router accepts a requested actor name, obtains its
  current worker assignment, and uses mTLS to `atunnel`. That hop authenticates
  router-to-worker traffic; it does not authenticate the original ingress
  client or hand an actor credential to the guest. AX debug guest operations
  therefore cannot be treated as a bootstrap authorization channel
  (`cmd/atenet/internal/router/ingress/ingress.go`,
  `internal/atunnel/ingress.go`).
- The pinned Substrate authentication guide says authorization/RBAC is not yet
  implemented. Its API bearer or pod-identity credential would grant far more
  than an attempt-scoped bootstrap. `MintActorCertificate` is limited to the
  atunnel purpose and has a pending authorization TODO. Do not mount a broad
  Substrate API credential or reuse the atunnel certificate as the agent's
  credential (`docs/authentication.md`, `cmd/ateapi/internal/controlapi/actor.go`).

## Required bootstrap boundary

The connector must verify cluster enrollment, current actor UID and assignment,
template/image/pool, attempt ownership, and effective network policy before
offering access. A guest must prove possession of an **activation-specific**
credential bound to that verified actor UID and the connector's challenge.
Task fields, a claimed UID, source IP, and a successful `ax ssh` call are not
proof. A bootstrap offer must carry no reusable secret until proof succeeds;
the exchange must be one-time, audience-bound, short-lived, replay-resistant,
and fenced against owner/actor replacement. The connector must recheck identity
and ownership before releasing any payload.

The preferred runtime spike is a small pre-workspace gate in the AX runner plus
an attestation operation on the trusted Substrate `atunnel` side. The runner
starts an authenticated bootstrap endpoint and waits before private Git setup
or a harness command. The connector challenges the activated actor through its
routed endpoint; the guest creates a fresh ephemeral encryption key **after**
receiving that challenge, so a golden snapshot cannot clone the key across
actors. `atunnel` must bind the challenge/key response to its current actor UID
and activation with verifiable evidence. Only then may the connector encrypt a
single-use setup payload for that guest. This is a candidate to test, not an
assumption that the current router supplies such evidence. The trusted
connector-to-router leg and bootstrap endpoint also require explicit client
authentication/authorization; Substrate's current ingress route is insufficient.

The pre-workspace gate must run during golden-template preparation without
starting private checkout or the harness, then complete only for the real
actor. Private Git access must exist before checkout; model access can wait
until after dependency setup. Do not put bootstrap tokens, Git URLs with
credentials, provider keys, or signer material in task YAML, template env,
guest metadata, logs, or snapshots. Resume requires a fresh proof and access
decision. Existing egress and AX `PoliciesApplied` status are inputs to
preflight, not substitutes for this identity exchange.

## Next proof

Implement the smallest atunnel-to-guest attestation surface and runner gate in
version-pinned overlays, then exercise it only with synthetic access. The test
must reject a forged actor name/UID, wrong cluster or audience, another attempt,
replayed/expired challenge, stale execution owner, cloned golden-snapshot key,
and actor replacement between challenge and delivery. It must prove private
checkout completes before command launch, failure blocks the command, and no
raw access survives into a snapshot or ordinary telemetry. Do not admit real
accounts or private repositories before these checks pass.
