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

`access.AuthorizeGitRead` is the first read-side decision. A trusted caller
provides the current organization, project, initiator, attempt, binding,
repository, commit, and policy version. The function locks the binding,
project policy, grant, connection, and provider registration in that order
inside the supplied PostgreSQL transaction. It requires an active approved
Git provider at the repository origin, an active connection, matching project
and grantee, an unrevoked/unexpired grant, unchanged grant and policy versions,
and a delivery mode allowed by both provider and project. It returns only
non-secret account metadata. The bootstrap connector's authorization callback
can call it using the transaction held through release delivery.

The [real PostgreSQL test](../internal/access/policy_test.go) covers a valid
decision, cross-tenant references, wrong project/attempt/grantee/repository/
commit, changed policy or grant versions, disabled Git use and delivery modes,
provider origin/state, grant expiry/revocation, and disabled connection. The
[bootstrap concurrency test](../internal/bootstrap/ledger_test.go) separately
proves a policy row locked in the release transaction cannot be updated until
that send completes.
The [secret-store test](../internal/access/secret_test.go) verifies ciphertext
at rest, tenant isolation, copied-ciphertext rejection, key-version reads, stale and concurrent rotations,
rotation blocked by an active credential read, expiry, disabled connections,
tampering, and caller-side clearing. It uses synthetic values only.

This is **not yet a usable account system**. There are no authenticated write
APIs, membership/RBAC checks for creating these rows, deployment key loading
and recovery, provider credential issuance, access leases, provider refresh or
revocation workers, or product scheduler wiring. The synthetic dev connector
still reads its root-owned fixture token. Those pieces must precede real
credentials.
