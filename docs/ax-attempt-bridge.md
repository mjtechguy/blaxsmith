# AX attempt bridge: first synthetic vertical proof

`internal/axbridge` maps one durable `workflow.Attempt` to one AX Task:
`blaxsmith-<organization UUID without dashes>/attempt-<attempt UUID without dashes>`.
The workflow reservation is committed before AX is called. The task contains
only a fixed synthetic shell command and a pinned runner image. It contains no
fence token, model key, Git credential, repository URL, or user command. The
AX CLI is restricted to a loopback tunnel because the pinned CLI uses plaintext
gRPC. A PostgreSQL transaction-scoped advisory lock serializes Launch,
ReconcileUnknown and StopKnown for the same attempt across scheduler processes.
The bridge reads the exact task spec back, then checks the live Substrate
actor, template UID, image, gVisor class, worker pod/pool, bootstrap signer and
data-only snapshot settings. It pins that runtime and command digest in the
workflow ledger before marking the attempt started.

An uncertain AX upsert moves the attempt to `reconciling`. Recovery makes no
new AX write: it requires exact task read-back and a matching live actor.
An absent task is **not** proof that a timed-out upsert cannot arrive later.
The attempt remains fenced; no replacement can be reserved. Cancellation of
an acknowledged attempt requires a caller-supplied bootstrap/lease revoker,
AX's two-phase delete, AX NotFound, and Substrate actor NotFound before
`ConfirmStopped`. An uncertain upsert cannot use this stop path until AX has a
server-side immutable create/tombstone or compare-and-delete contract.
The [pinned task-tombstone patch](ax-task-tombstones.md) is a tested proposal
for that contract; it is not deployed, and the current ephemeral AX Redis
would lose its tombstones on restart.

`RecoverySweep` now pages current `reserved` and `reconciling` owners after a
connector restart. It fences interrupted reservations, then performs one
bounded, read-only reconciliation per attempt. Missing or not-yet-running
actors remain visible as waiting and cannot stall the rest of the page. This
kernel is PostgreSQL/fake-AX tested, but no long-running connector invokes it
yet. A missing task remains an operator-resolution case because AX cannot prove
that an uncertain upsert will never arrive.

The dispatcher now requires an activator before it will reserve work. It copies
the validated Gateway's narrow egress allowlist into an attempt-named Gateway,
reads that definition back, and binds it alongside the attempt Workspace before
creating the Task. After AX/Substrate readback, it re-reads both bound resources
while the runner is still held behind the bootstrap gate, then passes the
persisted runtime binding and exact model grant to the activation callback. A
changed input fences the actor without releasing credentials. This removes the
shared-name dependency for a running attempt, but AX still permits privileged
writers to mutate those attempt resources and the readback does not prove the
Substrate dataplane. The signed release opens AX workspace setup; the runner
creates the model credential file only after local setup completes, though the
model payload is currently plaintext in runner memory during setup. Dispatch
then waits for AX `WorkspaceReady=True/SetupComplete` and rechecks the same
actor/runtime before returning `started`. A setup failure or timeout triggers
owner revocation and Task/actor-gone proof before retry. Stop removes the
Workspace and Gateway only after AX Task absence and Substrate actor absence
are proved. An uncertain activation remains fenced for reconciliation. The
callback is still not composed in the application or a connector process, so
product launch remains disabled.

`dispatch.ModelActivator` is the concrete model path: it creates a durable
bootstrap owner for the observed actor, builds the attempt-scoped
`NewModelAttemptConnector`, and opens the encrypted credential release. Its
preflight requires a private, certificate-verified router transport, a bounded
client timeout, a projected Substrate token, and a matching bootstrap signer.
`RevokeOwner` deactivates the stored owner and revokes all platform leases for
the attempt before actor deletion. Local lease revocation cannot invalidate a
raw credential already inside the worker; deleting the actor is required to
stop its use. The bootstrap connector can bundle a model grant with one exact
Git binding in the same release, but the dispatcher still does not build the AX
Workspace input from that binding. Assignment is not atomic with workflow
reservation, and the activator has not been wired into a long-running
connector or proven with product credentials on AX.

The dedicated node proof on 2026-09-23 used the existing dev PostgreSQL
database with a temporary schema and the authorized k3s node. The first run
exposed a missing `KUBECONFIG` in the probe environment; its deferred cleanup
removed the task, actor and schema. With `KUBECONFIG` set, the second run
passed: [record](ax-attempt-bridge-probe.json). An independent post-run AX
`GetTask` and Substrate `GetActor` both returned NotFound, and PostgreSQL
listed no `axbridge_%` schema. No credential or model work ran.

The follow-up [workspace-ready probe](ax-workspace-ready-probe.json) runs the
same bridge with a public Git Workspace and calls `WaitWorkspaceReady` without
releasing a credential. AX reached `Running` with the matching actor while
`WorkspaceReady=True/SetupComplete` was absent; the bridge stayed pending, then
cancelled and proved Task/actor absence before deleting the attempt resources.
To repeat on the node, build `deploy/dev/axbridge-probe`, set
`KUBECONFIG=/etc/rancher/k3s/k3s.yaml`, port-forward `ax-server` to loopback,
and pass the controller's pinned runner image/public key with
`--tool-inputs --workspace-ready`. This proves the fail-closed wait only; the
successful product activation-to-ready path is still open.

The pinned AX `Running` phase is a workload lifecycle status, **not** a
supervised command-exit result. During the first live run it reported `Running`
while `WorkspaceReady=False` and the bootstrap gate still blocked the command.
The gated runner now exposes a read-only command exit after `Wait`.
`CommandExitConnector` checks the exact AX task and live Substrate actor twice,
reads that endpoint through the connector-only route, verifies its activation
nonce against the latest durable bootstrap release for the current owner and
runtime, then signs an attempt-bound receipt. The workflow collector checks
the current attempt and immutable runtime binding before storing it. Exit code
alone never calls `FinishAttempt` or verifies artifacts. A guest file, AX
status, or `ax ssh` observation also cannot mark a task complete.
The runner reports one stable observation time, so repeating the same readback
replays the same signed receipt instead of generating a conflicting one.
The tenant-scoped `ListCommandExits` Connect API pages these immutable receipts
by their committed run event ID. It exposes the actor UID, connector signer ID, exit code, signal,
interruption flag, receipt digest, runner-reported time, and database receipt
time, but not the activation nonce or signature. A zero-exit receipt is a
necessary condition for a runtime-bound `FinishAttempt(..., true)`, never
sufficient proof of a result: a trusted verifier must still inspect the actual
artifact bytes and required checks before publishing the result digest. A
missing, nonzero, or interrupted receipt cannot mark that attempt successful.
A runtime-bound failed result also cannot release a live actor for retry.
`StopKnown` revokes its owner, deletes the AX task, and proves actor absence
before `ConfirmStopped` makes a replacement eligible. A failed exit remains
visible throughout; this read API does not perform the stop transition.

The command-exit route overlay, nonce binding, and connector are covered by
focused tests but have not been deployed or proven together on the node. The
runner and child share one container security boundary, so hostile same-UID
code could tamper with readback. Bootstrap owner assignment/revocation is not
yet atomic with workflow ownership. Exact replay works while the actor remains
available; losing it before a durable receipt still leaves the outcome unknown
and needs a separately proven recovery path. Product grants, independently
collected evidence, and mixed-tool
execution remain open. Only synthetic tasks are enabled here.
