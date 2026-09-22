# Blaxsmith

An enterprise web workspace for composable AI engineering teams, built on AX.

**Status:** planning and initial compatibility validation; application implementation has not started. The [main implementation plan](agent-factory-plan.md) contains the agreed architecture, product behavior, phased tasks, and acceptance criteria.

## Workspace layout

```text
forge-foundry-2/
├── blaxsmith/   # This Git repository; build the product here.
└── reference/   # Local AX, Guild, Coder, and Astronomer clones; outside the repo.
```

The private repository is [mjtechguy/blaxsmith](https://github.com/mjtechguy/blaxsmith). Reference clones are development inputs, not vendored source or runtime dependencies. Their inspected revisions and local source links are recorded in the plan. A clean clone of this repository does not include them.

## Planned deployment

- **Installed product:** Helm packages for the application and execution connector. One supported Linux/k3s node can host both; enterprise deployments can connect multiple execution clusters and external durable services.
- **Local development:** React/Vite and Go run locally, Compose supplies supporting services, and agent work runs on a real AX/Substrate execution cluster.
- **Cluster enrollment:** an authorized administrator registers a pool, installs the connector and validated runtime prerequisites, and completes scoped enrollment before it becomes eligible for work.

There is no custom Blaxsmith operator or Compose-based agent runtime in the initial scope. Helm charts, Compose files, and runnable application commands will be added during implementation; they do not exist yet.

## Beginning implementation

1. Complete the Phase 0 reference/version inventory and typed contracts (`P0-01/02`), plus the Guild and Astronomer adoption mappings (`P0-08/10`).
2. Prove secure AX bootstrap, credential boundaries, real application/test environments, and mixed-tool interaction (`P0-04/05/06/07/11`). Record supported versions and evidence before sensitive execution.
3. Build the Phase 1 application/API foundation and shared Astronomer frontend, then deliver the first recipe-backed workflow with evidence-first Q&A, steering, verification, and final human approval.

The first complete workflow uses a Claude architect, Codex implementer, and independently configured reviewer. Recipe comparisons follow in Phase 2; enterprise load/recovery proof follows in Phase 3. Work remains unchecked in the main plan until its acceptance evidence exists.
