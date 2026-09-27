# AX upgrade reconciliation

Target: `d0bc38bcf90bb2ad9c012ff1be9d68ff05347ba9`. Qualified runtime pin remains `f009cc81c9a571073bc1dd58cd2ed934bf2d5b1c`.

This records a local integration port on 2026-09-26. No target runtime has been adopted, deployed or live-qualified. The reference checkout was left clean. No source commits were created.

## Reproduction and result

The 15 ordered overlays in `integrations/ax/build.sh` were applied with the Git index to base `d8ed0fe38bceb7842d3c47817d53d16ccdfcb601` in a detached temporary worktree. Their combined patch was applied with `git apply --3way --index` to a second detached worktree at the target. Application returned exit 1 with conflicts in 11 files. Clean textual application elsewhere is not semantic compatibility proof.

Conflicted files:

- `demo.sh`
- `docs/roadmap.md`
- `docs/runner.md`
- `docs/sandbox.md`
- `internal/controller/reconciler.go`
- `internal/controller/reconciler_test.go`
- `internal/controller/worker_test.go`
- `internal/store/memory/store.go`
- `internal/store/redis/store.go`
- `internal/substrate/client.go`
- `internal/workspace/setup.go`

## Candidate implementation

The conflicts below have been resolved in [candidate-d0bc38b.patch](candidate-d0bc38b.patch), a single overlay applied directly to the target revision. The ordered overlays used by the default build remain unchanged.

- Immutable `CreateTask` is atomic in memory and Redis. Concurrent creates have one winner; caller-supplied status is discarded. Creation is suspended, with explicit ResumeTask/SuspendTask transitions. Permanent deletion tombstones still reject delayed creates, including delete-before-create. Task records do not expire automatically.
- Task-owned `spec.egress` replaces the removed Gateway resource. An absent/empty policy denies all egress. `allowAll: true` or valid IP CIDRs are the supported rules; ambiguous combinations, hostnames and malformed prefixes fail closed. The controller persists the actor policy before resume. This is a Blaxsmith overlay field, not an upstream AX feature.
- Status carries a lifecycle generation. Memory and Redis reject observations from older generations, so stale completion cannot overwrite a newer suspension. This does **not** fence outstanding Substrate RPC side effects; distributed actor ordering remains a qualification requirement.
- Suspend precedes template/network provisioning and treats an absent actor as already stopped. Failed and other non-running phases do not implicitly resume. Template hashes exclude mutable status and creation metadata.
- Workspace inline writes are checked, confined with Go `os.Root`, and fail initialization on absolute/traversing/symlink-escaping paths or filesystem failures. Credential-gated Git, bootstrap signer/image checks, provider credential delivery, signed command exit, resource bounds and queue ownership recovery are retained.

Reproduce without modifying the reference checkout:

```sh
integrations/ax/build-candidate.sh ../reference/ax /tmp/ax-candidate-build
```

The output directory must not exist. The script checks the exact clean reference revision, exports it, checks/applies the overlay, runs Go tests/vet and lifecycle race checks, builds four Linux/amd64 binaries, and writes hashes with `qualification: local-candidate-only`. It does not deploy or change the default runtime pin.

The local tests exercise actual gRPC transport, concurrent immutable creation, real Redis lifecycle/tombstone/recovery paths, workspace initialization failures, and existing bootstrap/credential/readback boundaries. Substrate RPCs use controlled fixtures; these checks are not live networking, golden-snapshot or provider qualification.

## Remaining ports and qualification

- Migrate the Blaxsmith connector from Gateway/upsert manifests to immutable task policy, exact creation readback and explicit lifecycle RPCs. The shipped connector still targets the qualified pin and must not be pointed at the candidate binaries.
- Prove actor-side ordering for overlapping resume/suspend/delete, late RPC responses, controller replacement and router-triggered resume. Store generation checks alone do not establish this property.
- Qualify the changed networking policy against the actual Substrate gateway and golden-snapshot path, including transitions to deny-all and denied bootstrap/provider routes.
- Requalify all bootstrap signer/image/phase checks, native credential delivery, signed exit readback, resource bounds and connector actor fencing after the port.

## Current artifact integrity

Combined patch SHA-256: `c8f1536e80cb79458ad5f4712bb25c9b46c8b0085af1d91d871fb7a8170b27bf`.

The hash above identifies the original combined input to the three-way experiment, before conflict resolution. The candidate build records the resolved overlay hash in its own provenance. The temporary worktrees are development aids; the checked-in candidate patch and build script reproduce the port. The live fault matrix remains required before changing the qualified pin.
