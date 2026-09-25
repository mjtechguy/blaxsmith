# First access-authority slice

`0004_access_authority.sql` stores non-secret provider registrations,
connections, project policy, grants, and frozen attempt bindings. Composite
organization keys and foreign keys prevent references into another tenant.
A binding records one exact Git repository and commit, the grant version, and
the project policy version resolved for an attempt. Neither a connection nor
a grant is a worker credential or a lease.

`0005_access_secrets.sql` adds versioned AES-256-GCM ciphertext and a current
version pointer on each connection. Encryption keys are supplied to
`SecretStore` from trusted configuration outside PostgreSQL. Associated data
binds organization, connection, version, and key ID; a random nonce is used
for every write. Rotation locks the connection and compares the version last
seen by the refresh owner. One concurrent refresh wins; the other must read
current state before retrying. `ReadCurrent` locks the connection and version
row in the caller's release transaction, checks expiry and active state, and
returns a byte slice the caller must clear. Missing old keys or tampered
ciphertext fail closed. Old keys must remain recoverable while retained rows
or backups require them.

`0150_organization_data_keys.sql` adds envelope encryption. Each organization
gets its own random 256-bit data key, stored only wrapped by a master key with
associated data that binds organization, data-key version, and master key ID.
Secrets are sealed by their organization's data key, so a data key opens no
other organization's secrets. Rows sealed directly by a master key before 0150
(`data_key_version IS NULL`) stay readable. They are re-encrypted on the next
write for their connection, and `serve-app` upgrades the rest in batches at
startup. `blaxsmith admin upgrade-secrets` runs the same idempotent pass and
reports what remains.

### Rotating the master key

Master keys come from `BLAXSMITH_ACCESS_KEY_FILE` (the current 32-byte key),
`BLAXSMITH_ACCESS_KEY_ID` (its ID, default `primary`), and
`BLAXSMITH_ACCESS_PREVIOUS_KEY_FILES` (optional `id=path,id=path` older keys
kept for decryption). Any malformed entry, duplicate ID, missing or
group/world-readable file, or wrong key length stops startup. In the Helm chart
these are `accessKeySecretName`, `accessKeyID`, and `previousAccessKeys`
(`[{id, secretName}]`, each Secret holding the raw key under `key`).

1. Create a new 32-byte key in a new Secret and pre-stage it: add it to
   `previousAccessKeys` under a new ID (for example `key-2`) and roll out, so
   every app and gateway pod can unwrap with either key before any data key
   moves.
2. Swap: point `accessKeySecretName` at the new Secret with `accessKeyID: key-2`,
   and list the old key under its old ID in `previousAccessKeys` (the first
   key's ID is `primary`). Roll out. New data keys and writes use the new
   key; each write re-wraps its organization's data key, and `serve-app`
   re-wraps all of them at startup. Only data keys are re-wrapped; secret
   ciphertext is not re-encrypted.
3. Run `blaxsmith admin upgrade-secrets` with the same environment. It must
   report `0 data keys under a non-current master key, 0 legacy rows remaining`.
4. Remove the old key from `previousAccessKeys` and roll out. Keep the old key
   offline for as long as backups taken before step 3 must stay restorable.

`0006_access_leases.sql` records an attempt, actor UID, owner generation,
binding, connection, audience, resource, secret version, expiry, and delivery
state. `0025_multi_capability_leases.sql` permits one lease per capability on a
challenge, so Git and model credentials can share one signed encrypted release
while retaining separate bindings and revocation. Each lease is reserved in
the transaction that commits bootstrap release intent, before network delivery.
The send transaction checks current authority, reads each secret, and marks
each lease attempted and delivered only after the guest acknowledges release.
If the send outcome or commit is uncertain, the reservations and bootstrap
challenge's durable attempted state remain for reconciliation; the same
challenge cannot be retried. Resume gets a new challenge and leases.
`RevokeGrant` and `RevokeConnection` block later decisions
and mark their recorded leases revoked under the same database transaction;
revocation waits for an in-progress release holding the relevant row locks.
**Lease expiry or revocation does not invalidate a raw bearer
token already copied into a sandbox or at its provider.**

`access.AuthorizeGitRead` is the first read-side decision. A trusted caller
provides the current organization, project, initiator, attempt, binding,
repository, commit, and policy version. The function locks the binding,
project policy, grant, connection, and provider registration in that order
inside the supplied PostgreSQL transaction. It requires an active approved
Git provider at the repository origin, an active connection, matching project
and grantee, an unrevoked/unexpired grant, unchanged grant and policy versions,
and a delivery mode allowed by both provider and project. It returns only
non-secret account metadata. The bootstrap connector's authorization callback
calls it for Git alongside the model authorization, using the transaction held
through release delivery. `GitSetup` and `ModelCredential` read their encrypted
secrets under that same transaction; the runner receives them only in its
signed envelope.

The [real PostgreSQL test](../internal/access/policy_test.go) covers a valid
decision, cross-tenant references, wrong project/attempt/grantee/repository/
commit, changed policy or grant versions, disabled Git use and delivery modes,
provider origin/state, grant expiry/revocation, and disabled connection. The
[bootstrap concurrency test](../internal/bootstrap/ledger_test.go) separately
proves a policy row locked in the release transaction cannot be updated until
that send completes. The [live synthetic probe](bootstrap-private-git-probe.json)
checked out the exact private commit before command launch and after a fresh
resume release, using the seeded binding and encrypted database token. Its
[bounded scan](bootstrap-private-git-secret-scan.json) found no plaintext in
the inspected database, AX state, logs, or evidence files.
The [secret-store test](../internal/access/secret_test.go) verifies ciphertext
at rest, tenant isolation, copied-ciphertext rejection, key-version reads, stale and concurrent rotations,
rotation blocked by an active credential read, expiry, disabled connections,
tampering, and caller-side clearing. It uses synthetic values only.
The [lease test](../internal/access/lease_test.go) covers actor binding,
duplicate reservation, delivery state, rollback after a possible send, and
grant and connection revocation, including a connection update blocked by a
live authority-read transaction. The [live report](bootstrap-private-git-probe.json) confirms
two distinct delivered lease records with the current actor and owner
generations across data-snapshot resume.

This is **not yet a usable account system**. There are no authenticated write
APIs, membership/RBAC checks for creating these rows, deployment key loading
and recovery, provider credential issuance, lease renewal/reconciliation,
provider refresh or revocation workers, or product scheduler wiring. The dev
seed command reads a
root-owned fixture token once; the release connector reads only ciphertext
through `SecretStore`. Those pieces must precede real
credentials.
