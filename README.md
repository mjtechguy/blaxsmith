# Blaxsmith

An enterprise web workspace for composable AI engineering teams, built on AX.

**Status:** implementation started. Git-backed recipes compile into immutable input bundles and run Guild's pinned Forge validator. A read-only web preview lists current Codex, Claude Code, and OpenCode publisher releases. Identity, the scheduler, and tool execution adapters are still being built. The [main implementation plan](agent-factory-plan.md) contains the agreed architecture, phased tasks, and acceptance criteria.

## Run the first slice

Requires Go 1.27.1, Git, and Python 3.10+.

```sh
make check
make build
make example
```

The [Git recipe contract](docs/git-recipes.md) explains composition, frozen inputs,
`AGENTS.md`/skill handling, and bundle export. See the [Phase 0 evidence](docs/phase-0-baseline.md)
for source pins, Guild reuse, runtime findings, and remaining validation.

A dedicated remote k3s/Substrate/AX development node has passed a synthetic
command and suspend/resume file-persistence test. Its [runbook](deploy/dev/README.md)
records the tested setup; this is not yet the product installation or a secure
multi-tenant worker environment.

The [AX compatibility patch](integrations/ax/README.md) now blocks observed
startup failures, enforces the tested new-connection egress path, and adds a
synthetic pre-workspace release gate. The [live bootstrap probe](docs/bootstrap-gate-probe.json)
passed release and replay checks across suspend/resume. A separate
[actor proof](docs/actor-attestation-probe.json) now validates the current
actor UID and guest challenge against the Substrate cluster CA. The
[router probe](docs/bootstrap-router-auth-probe.json) also verifies HTTPS and
an audience-scoped connector identity before bootstrap. A
[PostgreSQL challenge ledger](docs/bootstrap-ledger.md) now verifies and
consumes an attempt/owner-bound proof once. A [live synthetic connector
probe](docs/bootstrap-ledger-release-probe.json) exercised release and owner
deactivation through PostgreSQL and the current actor. A subsequent
[synthetic private Git probe](docs/bootstrap-private-git-probe.json) delivered
an encrypted setup token and checked out a private HTTPS fixture before the
task command, including a fresh release after data-snapshot resume. Product
policy, real connection custody, effective egress enforcement, and revocation
remain prerequisites for sensitive work.
The first [non-secret access-authority schema and Git-read check](docs/access-authority.md)
now lock current provider, connection, project policy, grant, and binding rows
through the release transaction. A versioned encrypted secret store and
attempt-bound delivery leases now pass synthetic PostgreSQL tests and a live
private-Git checkout. Account onboarding, provider issuance, lease renewal,
and provider revocation are still unimplemented.
The [actor-UID fence](docs/bootstrap-actor-fence-proof.json) now rejects a
stale target at both router and worker ingress in the dev cluster.
The [controller-owned bootstrap key](docs/bootstrap-platform-key-probe.json)
also blocks Task-supplied signers and unapproved runner images before launch.

Recipes now accept Claude Code, Codex, and OpenCode profiles. The updated plan
includes a recent tool-version catalog with latest stable selected for new
installations and exact runtime pins afterward. The first [read-only catalog
command](docs/tool-runtime-catalog.md) now lists recent publisher-backed CLI
versions and their exact package integrity metadata with `blaxsmith tools`.
The [catalog web preview](docs/frontend-adoption.md) lists these releases but cannot approve or install them. Verified installation and launch adapters are not implemented yet.

## Run the web preview

Requires Go 1.27.1 and Node 24.21+ (before Node 25). In separate terminals:

```sh
go run ./cmd/blaxsmith serve
```

```sh
cd frontend
npm ci
npm run dev
```

Open `http://127.0.0.1:3000/tools`. The API binds only to `127.0.0.1:8001`; Vite proxies `/api` in development. `make web-check` builds the frontend and checks its types. `make proto-check` lints and regenerates the pinned Connect Go/TypeScript contract and rejects drift. This preview contains public release metadata only and has no login or access to product records.

## Local PostgreSQL

The local Compose service runs a digest-pinned PostgreSQL 18 image on
`127.0.0.1:55434` with a persistent volume. It is a supporting service, not an
agent runtime. Create a local-only password and start it:

```sh
umask 077
printf 'BLAXSMITH_DEV_DB_PASSWORD=%s\n' "$(openssl rand -hex 24)" > .env
docker compose up -d --wait postgres
set -a
. ./.env
set +a
export BLAXSMITH_DATABASE_URL="postgres://blaxsmith:${BLAXSMITH_DEV_DB_PASSWORD}@127.0.0.1:55434/blaxsmith?sslmode=disable"
go run ./cmd/blaxsmith migrate
BLAXSMITH_TEST_DATABASE_URL="$BLAXSMITH_DATABASE_URL" make check
```

`migrate` embeds the numbered SQL files, runs them once under a PostgreSQL lock,
and rejects changed or unknown applied versions. Use `docker compose down` to
stop the service without deleting its data. The `.env` file is ignored by Git;
the password applies when the volume is first initialized.

The operator can create the installation's first local owner after migration:

```sh
go run ./cmd/blaxsmith bootstrap-owner --username alice \
  --organization-slug example --organization-name "Example"
```

It reads and confirms the password from the terminal and atomically creates
one principal, organization, owner membership, and audit event. The internal
session service now verifies local credentials, issues ten-minute signed access
tokens, rotates hashed refresh tokens, checks current membership/policy on each
request, and revokes on refresh replay. Login attempts are counted in PostgreSQL
across replicas before bounded password hashing; identity state is rechecked
under a lock before a session is issued. A generated Connect browser handler
now tests HTTPS, exact origin, CSRF, secure HttpOnly cookies, refresh, and logout;
it is not mounted by the development API or deployed. MFA, key custody, account
management, trusted client-address handling behind ingress, and scheduled
pruning of old login-limit keys remain release work. The
[guide adoption record](docs/adr/0001-technology-guide.md)
tracks exact baseline choices and current deviations.

## Workspace layout

```text
forge-foundry-2/
├── blaxsmith/   # This Git repository; build the product here.
└── reference/   # Local AX, Guild, Coder, and Astronomer clones; outside the repo.
```

The private repository is [mjtechguy/blaxsmith](https://github.com/mjtechguy/blaxsmith). Reference clones remain outside the product repository. The selected standalone Guild validator is included under `internal/guild/upstream` with its original license and provenance; a clean clone needs no reference directories.

## Planned deployment

- **Installed product:** Helm packages for the application and execution connector. One supported Linux/k3s node can host both; enterprise deployments can connect multiple execution clusters and external durable services.
- **Local development:** React/Vite and Go run locally, Compose supplies supporting services, and agent work runs on a real AX/Substrate execution cluster.
- **Cluster enrollment:** an authorized administrator registers a pool, installs the connector and validated runtime prerequisites, and completes scoped enrollment before it becomes eligible for work.

There is no custom Blaxsmith operator or Compose-based agent runtime in the initial scope. A [preview Helm chart](deploy/charts/blaxsmith-preview/README.md) packages only the read-only catalog UI/API; authenticated product and connector charts remain planned. The recipe CLI, public catalog API, and local web preview described above are runnable now.

## Beginning implementation

1. Complete the Phase 0 reference/version inventory and typed contracts (`P0-01/02`), plus the Guild and Astronomer adoption mappings (`P0-08/10`).
2. Prove secure AX bootstrap, credential boundaries, real application/test environments, and mixed-tool interaction (`P0-04/05/06/07/11`). Record supported versions and evidence before sensitive execution.
3. Build the Phase 1 application/API foundation and shared Astronomer frontend, then deliver the first recipe-backed workflow with evidence-first Q&A, steering, verification, and final human approval.

The first complete workflow uses a Claude architect, Codex implementer, and independently configured reviewer. Recipe comparisons follow in Phase 2; enterprise load/recovery proof follows in Phase 3. Work remains unchecked in the main plan until its acceptance evidence exists.
