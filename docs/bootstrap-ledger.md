# Bootstrap challenge ledger

The migrations and `internal/bootstrap` implement the durable owner/challenge
fence for P0-04. A synthetic connector uses it to release the AX runner and
deliver a synthetic private-Git setup token under an attempt-bound platform
lease; this is not yet a user-authorized product lease.

The scheduler owns one `bootstrap_owners` row per cluster/attempt. `Assign`
creates its first generation or replaces an owner only when the caller supplies
the current generation. `Deactivate` likewise compares and advances the
generation; a stale scheduler call cannot stop a replacement. These are
database fences, not scheduler authorization: only the future trusted
scheduler may call them. A caller of `Issue` must already be authorized for
that cluster and attempt; `Scope` is an input, not authentication. The trusted
caller supplies the enrolled cluster CA to `Redeem`; neither the worker nor
request body selects it.

`Issue` locks the active owner row, cancels any pending challenge for that
attempt, and stores a random challenge ID and SHA-256 nonce verifier. The raw
nonce is returned once to the caller and is never stored. The offer expires
after two minutes. `Redeem` locks that owner and the challenge, requires the
same cluster, attempt, generation, actor atespace/name/UID, nonce, and
unexpired unused offer, then verifies the signed AX guest challenge against
the enrolled CA. It atomically records consumption; replay or a concurrent
second redemption is denied. Owner transfer and redemption serialize on the
owner row. Issuing a replacement offer cancels the previous pending one and
supersedes an already redeemed proof that has not completed release.

Redeeming is **proof, not permission**. `Release` first commits an irrevocable
release attempt, then holds the current owner row while the connector checks
the live actor, template UID, pinned image, worker pod UID/pool, signer key,
and an authorization callback. It signs the guest challenge only after these
checks. The authorization and credential-selection callbacks now receive the
same PostgreSQL transaction used through the send. A caller can lock current
grant, binding, and connection rows there so revocation serializes with
release; the focused PostgreSQL test proves a policy-row update waits for
the send transaction and a later release sees revocation. The synthetic dev
callback at that stage checked only runtime properties. A failed or uncertain send cannot retry the same redemption; a new
challenge is required. The connector sends the ledger actor UID through the
authenticated HTTPS router, which fences the receiving `atunnel` activation.
The ledger stores a SHA-256 verifier for the runner activation nonce at redeem,
then pins the actor template UID, image, and worker pool in the release
transaction after HTTP 204. Command-exit readback accepts only the latest
released nonce under the current active owner and exact runtime. Earlier
released rows without this binding fail closed.
The [live synthetic probe](bootstrap-ledger-release-probe.json) passed initial
and data-snapshot-resume releases, replay rejection, and owner deactivation.
PostgreSQL recorded two consumed/release-attempted/released challenges and an
inactive generation-3 owner. That first run carried no credential.

The next [private-Git probe](bootstrap-private-git-probe.json) included an
X25519 guest key in the actor-signed challenge. After the final runtime check,
the connector encrypted a synthetic token for that key with AES-GCM and signed
the ciphertext hash into the one-use release. The runner handed the token only
to Git's askpass during the matching HTTPS fetch; it was absent from the Task,
Workspace, ActorTemplate, Git config, command environment, and inspected logs.
The private checkout and data-snapshot resume passed. The connector now
rejects a template unless pause and commit use data-only snapshots, resume
starts from the golden image, and the dev storage location matches the
configured snapshot bucket. The [live rerun](bootstrap-private-git-probe.json)
also observed data-only external snapshots after both suspensions. A [bounded
surface scan](bootstrap-private-git-secret-scan.json) found no token in 177 AX Redis
keys, eight runtime container logs, the dev platform database, fixture logs,
or nine probe evidence files. The owner ended inactive at generation 3.

The latest synthetic probe seeds provider, connection, project policy, grant,
and binding records before task launch, encrypts the fixture token in a
versioned database row, and reads it only after a row-locked Git-read decision
in the release transaction. The key remains in an owner-only file outside the
database. A distinct lease reservation is committed with each release intent;
the [live report](bootstrap-private-git-probe.json) confirms delivery to the
same actor under owner generations 1 and 2. This tests the access path but
does not provide authenticated onboarding or a real provider credential.
The product scheduler must still own `Assign`/`Deactivate`, authenticate the
connector, enforce real membership/RBAC and measured effective
isolation/egress, and issue and revoke provider-scoped Git access. The current
synthetic raw token can remain provider-valid after its platform lease expires
or is revoked. The synthetic connector supplies a pinned
Git commit inside the encrypted release; the runner rejects a fetch that does
not resolve to that commit before checkout, and the live fixture matched it.
The product must derive that pin from an authorized frozen input bundle. The
probe's image-owned private CA, fixture token, and dev key are not production
provider integration or key management. The resume marker is inside the workspace and checked for
the expected repository URL and commit, but is not a signed provenance record. Full
snapshot and memory/alternate-egress/recovery-generation tests remain open.
Restored database state must not resurrect old authority.

The focused test uses a real PostgreSQL instance. Set
`BLAXSMITH_TEST_DATABASE_URL` to a disposable database where the test user can
create schemas, then run `go test -race -count=1 -run TestLedgerPostgres
./internal/bootstrap`. The test creates a temporary schema and checks durable
redeem from a second pool, cancellation, expiration, wrong scope/nonce/CA,
replay, concurrent redemption, changed owner generation or actor UID, an
inactive owner, release replay, unknown send outcome, and superseded redemption.
It also checks duplicate, stale, and concurrent scheduler assignments. Without
that environment variable, `make check` compiles the test but skips the
database exercise. It also proves a row-locked policy check remains fenced
through send. `TestConnectorDoesNotFollowRedirectWithToken` runs without
PostgreSQL and rejects redirect-based connector-token forwarding.
