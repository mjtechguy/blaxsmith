# Bootstrap challenge ledger

The migrations and `internal/bootstrap` implement the durable owner/challenge
fence for P0-04. A synthetic connector now uses it to release the AX runner;
it does not issue credentials.

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
inactive generation-3 owner. No secret or private repository was used.

The dev CLI's authorization callback checks only synthetic runtime properties.
The product scheduler must still own `Assign`/`Deactivate`, authenticate the
connector, enforce current grant/binding/policy and measured effective
isolation/egress, bind encrypted delivery to a fresh guest key, and implement
lease revocation. Full-snapshot, recovery-generation, and private-Git probes
remain pending. Restored database state must not resurrect old authority.

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
