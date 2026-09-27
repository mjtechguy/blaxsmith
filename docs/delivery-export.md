# Delivery snapshots

Run → Review → Prepare delivery export produces a downloadable Markdown snapshot. The same `GetDeliveryReport` operation is available to authenticated browser users and project-scoped machine/MCP readers.

The report reads one repeatable-read PostgreSQL snapshot: source and bundle digests, the run's frozen check policy, current acceptance package and decision, associated goal/plan, stage states, and an index of current and historical evidence. Its response contains the Markdown SHA-256 and generation time. The download filename includes the digest.

A successful stage is not a completed requirement. The associated plan is labeled project-authored intent. A missing review package, unmeasured token usage, unknown cost, and unestablished merge/deployment remain explicit. Off checks never appear as passed. Historical failures remain listed after successful corrections. A later review decision can supersede an exported snapshot.

Raw logs, artifact bodies, commands, runtime configuration and platform credentials are excluded. The original goal and plan can contain private project content; review them before sharing. Evidence IDs require authenticated access to the originating project and depend on its retention policy. This is a reference export, not a portable archive of evidence bodies.

Exports fail explicitly beyond 5,000 evidence records or 2 MiB; paginated evidence APIs remain available. No partial report is presented as complete. Provider publication, merge, deployment, measured usage and per-requirement implementation proofs are separate work.

`make e2e` verifies real browser download and digest, machine API access, and accepted/unaccepted/correction histories in the local Git + PostgreSQL execution fixture. It does not claim live AX or provider qualification.

Usage exports now include immutable harness-reported subtotals and attempt/report coverage in the same snapshot. Missing token or cost data is explicitly unknown. No complete-measurement, invoice, subscription quota or price-version claim is made; see [reported usage](usage-reporting.md).
