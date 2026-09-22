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
