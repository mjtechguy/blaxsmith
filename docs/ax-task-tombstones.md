# AX task-name tombstones: proposed upstream patch

The [patch](../integrations/ax/task-tombstones.patch) applies to
AX commit `d8ed0fe38bceb7842d3c47817d53d16ccdfcb601`. It has **not** been
deployed to the shared k3s node.

AX currently handles `UpdateTask` as an unconditional upsert. A request that
times out at the Blaxsmith connector can reach AX after `DeleteTask`, recreate
the task, and start a new actor. Blaxsmith already fences uncertain attempts in
PostgreSQL, but that alone cannot stop AX from running the resurrected task.

The patch makes a task name single-use. Redis scripts atomically check a
permanent tombstone before saving a task or status. `DeleteTask` writes the
tombstone even if the task has not yet arrived, then marks any existing task
`Terminating`; controller cleanup removes the record but retains the marker.
The memory store mirrors this contract for single-process use. `UpdateTask`
returns `FailedPrecondition` for a deleted name. A retry must use a **new**
attempt ID and task name. Deleting a missing name still returns `NotFound`,
but permanently fences it.

To verify on a clean AX checkout at the pinned commit:

```sh
git apply --check /path/to/blaxsmith/integrations/ax/task-tombstones.patch
git apply /path/to/blaxsmith/integrations/ax/task-tombstones.patch
go test ./...
```

The Redis integration test starts a local `redis-server` and checks that an
upsert cannot revive a name after a missing-task delete, during two-phase
cleanup, or after record removal. It also checks that stale status cannot
replace `Terminating`, the marker has no TTL, and a new attempt name works.
The AX API test checks the same name contract through `UpdateTask` and
`DeleteTask`. These tests passed locally on the pinned source.

**Adoption gate:** AX's current `deploy/redis.yaml` uses an ephemeral Redis
container with no persistent volume. Its tombstones disappear on Redis restart.
Before enabling cancellation of an AX attempt with an uncertain upsert, deploy
a durable no-eviction store for tombstones (for example persistent Redis with
AOF and a suitable fsync policy), roll all AX API replicas to the patched
version, and verify the delete-absent/upsert-late probe across a server and
Redis restart. Existing deletes made before the rollout have no tombstone.
Blaxsmith's `ReconcileUnknown` must remain read-only and fenced until this
contract is deployed and observed. The patch does not itself wire a
`StopUnknown` path, provision durable Redis, or change AX's plaintext gRPC
transport.
