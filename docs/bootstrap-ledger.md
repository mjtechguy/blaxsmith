# Bootstrap challenge ledger

`db/migrations/0001_bootstrap.sql` and `internal/bootstrap/ledger.go` are the
first durable piece of P0-04. They do not issue credentials or release the AX
runner.

The scheduler owns one `bootstrap_owners` row per cluster/attempt. It must
advance `owner_generation` and replace the actor identity when assigning a new
execution owner, or set `active=false` when ending the attempt. These writes
must use the same durable authority domain as the ledger. A caller of `Issue`
must already be authorized for that cluster and attempt; `Scope` is an input,
not authentication. The trusted caller supplies the enrolled cluster CA to
`Redeem`; neither the worker nor request body selects it.

`Issue` locks the active owner row, cancels any pending challenge for that
attempt, and stores a random challenge ID and SHA-256 nonce verifier. The raw
nonce is returned once to the caller and is never stored. The offer expires
after two minutes. `Redeem` locks that owner and the challenge, requires the
same cluster, attempt, generation, actor atespace/name/UID, nonce, and
unexpired unused offer, then verifies the signed AX guest challenge against
the enrolled CA. It atomically records consumption; replay or a concurrent
second redemption is denied. Owner transfer and redemption serialize on the
owner row. Issuing a replacement offer cancels the previous one.

The return value is **proof of one successful redemption, not permission to
release access**. Before release or credential delivery, the connector must
requery the live AX/Substrate actor assignment and execution owner, enforce
current grant/binding/policy and effective isolation/egress, bind the payload
to the fresh guest encryption key, and fence owner replacement through
delivery. Product connector authentication, the platform release signer,
encrypted private-Git payload, lease lifecycle, recovery generation, and
snapshot/revocation probes are still pending. A restored database must not
resurrect old bootstrap authority.

The focused test uses a real PostgreSQL instance. Set
`BLAXSMITH_TEST_DATABASE_URL` to a disposable database where the test user can
create schemas, then run `go test -race -count=1 -run TestLedgerPostgres
./internal/bootstrap`. The test creates a temporary schema and checks durable
redeem from a second pool, cancellation, expiration, wrong scope/nonce/CA,
replay, concurrent redemption, changed owner generation or actor UID, and an
inactive owner. Without that environment variable, `make check` compiles the
test but skips the database exercise.
