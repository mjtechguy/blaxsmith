# First access-authority slice

`0004_access_authority.sql` stores non-secret provider registrations,
connections, project policy, grants, and frozen attempt bindings. Composite
organization keys and foreign keys prevent references into another tenant.
A binding records one exact Git repository and commit, the grant version, and
the project policy version resolved for an attempt. Neither a connection nor
a grant is a worker credential or a lease.

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

This is **not yet a usable account system**. There are no authenticated write
APIs, membership/RBAC checks for creating these rows, encrypted secret
versions, provider credential issuance, access leases, refresh or revocation
workers, or product scheduler wiring. The synthetic dev connector still reads
its root-owned fixture token. Those pieces must precede real credentials.
