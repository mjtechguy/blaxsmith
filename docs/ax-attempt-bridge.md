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
data-only snapshot settings before marking the attempt started.

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

The dedicated node proof on 2026-09-23 used the existing dev PostgreSQL
database with a temporary schema and the authorized k3s node. The first run
exposed a missing `KUBECONFIG` in the probe environment; its deferred cleanup
removed the task, actor and schema. With `KUBECONFIG` set, the second run
passed: [record](ax-attempt-bridge-probe.json). An independent post-run AX
`GetTask` and Substrate `GetActor` both returned NotFound, and PostgreSQL
listed no `axbridge_%` schema. No credential or model work ran.

The pinned AX `Running` phase is a workload lifecycle status, **not** a
supervised command-exit result. During the first live run it reported `Running`
while `WorkspaceReady=False` and the bootstrap gate still blocked the command.
The runner has an in-process `OnCommandExit` hook, but it does not publish a
trusted, attempt-bound result to the control plane. A guest file, AX status,
or an `ax ssh` observation must not call `FinishAttempt`. The minimum acceptance
protocol needs an outside-guest supervisor or equivalent trusted sender to
emit a signed event bound to organization/run/task/attempt IDs, owner generation,
fence token digest, AX task identity, actor UID, template UID, command digest,
exit code and monotonic event sequence. The scheduler must recheck the current
owner and frozen verification policy before accepting that event. This remains
open, as do server-side AX idempotent create/tombstones, bootstrap owner
revocation wiring, and product grants. Only synthetic tasks are enabled here.
