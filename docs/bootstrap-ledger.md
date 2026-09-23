# Bootstrap challenge ledger

The migrations and `internal/bootstrap` implement the durable owner/challenge
fence for P0-04. A synthetic connector uses it to release the AX runner and
deliver a synthetic private-Git setup token; it does not issue product leases.

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
checks. A failed or uncertain send cannot retry the same redemption; a new
challenge is required. The connector sends the ledger actor UID through the
authenticated HTTPS router, which fences the receiving `atunnel` activation.
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
The private checkout and data-snapshot resume passed. A [bounded surface
scan](bootstrap-private-git-secret-scan.json) found no token in 121 AX Redis
keys, eight runtime container logs, the dev bootstrap database, fixture logs,
or nine probe evidence files. The owner ended inactive at generation 3.

The dev CLI's authorization callback checks only synthetic runtime properties.
The product scheduler must still own `Assign`/`Deactivate`, authenticate the
connector, enforce current grant/binding/policy and measured effective
isolation/egress, issue and revoke short-lived Git access from an authorized
Connection/Grant/Binding/Lease, and verify exact repository revision. The
probe's image-owned private CA and root-only synthetic token are not product
credential custody. The resume marker is inside the workspace and checked for
the expected repository URL, but is not a signed provenance record. Full
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
database exercise. `TestConnectorDoesNotFollowRedirectWithToken` runs without
PostgreSQL and rejects redirect-based connector-token forwarding.
