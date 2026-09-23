# Blaxsmith

An enterprise web workspace for composable AI engineering teams, built on AX.

**Status:** implementation started. The first working slice compiles Git-backed engineering recipes into immutable input bundles and runs Guild's pinned Forge validator. The web workspace, scheduler, and tool adapters are not implemented yet. The [main implementation plan](agent-factory-plan.md) contains the agreed architecture, phased tasks, and acceptance criteria.

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
The [actor-UID fence](docs/bootstrap-actor-fence-proof.json) now rejects a
stale target at both router and worker ingress in the dev cluster.
The [controller-owned bootstrap key](docs/bootstrap-platform-key-probe.json)
also blocks Task-supplied signers and unapproved runner images before launch.

Recipes now accept Claude Code, Codex, and OpenCode profiles. The updated plan
includes a recent tool-version catalog with latest stable selected for new
installations and exact runtime pins afterward. The catalog UI and launch
adapters are not implemented yet.

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

There is no custom Blaxsmith operator or Compose-based agent runtime in the initial scope. Product Helm charts, Compose files, and web/API commands will be added during implementation; they do not exist yet. The recipe CLI and development runtime inputs described above are runnable now.

## Beginning implementation

1. Complete the Phase 0 reference/version inventory and typed contracts (`P0-01/02`), plus the Guild and Astronomer adoption mappings (`P0-08/10`).
2. Prove secure AX bootstrap, credential boundaries, real application/test environments, and mixed-tool interaction (`P0-04/05/06/07/11`). Record supported versions and evidence before sensitive execution.
3. Build the Phase 1 application/API foundation and shared Astronomer frontend, then deliver the first recipe-backed workflow with evidence-first Q&A, steering, verification, and final human approval.

The first complete workflow uses a Claude architect, Codex implementer, and independently configured reviewer. Recipe comparisons follow in Phase 2; enterprise load/recovery proof follows in Phase 3. Work remains unchecked in the main plan until its acceptance evidence exists.
