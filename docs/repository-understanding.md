# Repository baselines and behavior maps

Every newly frozen recipe records a repository baseline at its exact source commit and scope. Launch preview shows the detected mode, file count, instruction/manifest paths, proposed checks, and limitations. Worker context includes the same baseline. Detection reads bounded committed files and never executes scripts or installs dependencies.

The initial mode heuristic reports brownfield when source files or manifests exist in scope, mixed when a new scoped component sits inside an existing repository, and greenfield when no known implementation is detected. It is explicitly a heuristic. A project inputs file may set `project_mode` to `greenfield`, `brownfield`, or `mixed`; the selection and detected evidence are retained separately. Existing package-manager pins and engine constraints are preserved. Suggested checks are not selected checks and never count as passing evidence. Native planning's verification stage still executes only the separately frozen project policy.

An uninitialized remote without a commit still needs authorized source bootstrap before an AX run can clone it. A goal conversation and imported plan can exist before source bootstrap. No synthetic default-branch history is invented by this feature.

## Durable scoped knowledge

Commit a compact behavior map and select its path in a [project inputs](project-inputs.md) document:

```json
{"schema_version":"blaxsmith.inputs/v1alpha1","project_mode":"brownfield","knowledge":["docs/knowledge/settings.json"]}
```

A snapshot has this shape (replace the example hashes with actual SHA-256 file hashes and the observed Git commit):

```json
{
  "schema_version": "blaxsmith.knowledge/v1alpha1",
  "title": "Settings update flow",
  "source_commit": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "scope": "internal/settings",
  "dependencies": {
    "internal/settings/service.go": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  },
  "claims": [{
    "id": "update-entry",
    "statement": "The update handler validates the caller before writing settings.",
    "kind": "observed",
    "locations": [{"path":"internal/settings/service.go","line":42}]
  }],
  "uncertainties": ["Concurrent update behavior has not been tested."]
}
```

Map the entry point, important callers, interfaces, storage, authorization, shared helpers and relevant checks at the breadth the task warrants. Classify statements as `observed` or `inferred`; both remain authored claims. Cite locations and include every file dependency that can invalidate the conclusion. Record uninspected behavior explicitly. Source attribution and matching file hashes do not prove a claim's truth or completeness.

The freezer compares declared dependencies against the current pinned Git tree. Unrelated edits preserve freshness. Changed dependencies mark the snapshot **stale**; missing/unreadable dependencies mark it **unavailable**. Stale/unavailable claim text is excluded from worker context, while paths and status remain visible for refresh. The original snapshot bytes and hashes remain in the bundle. A correction or review attempt on a candidate different from the frozen source conservatively receives **not revalidated for candidate**, with claims omitted. This avoids presenting original-source observations as verified facts about later code.

Snapshot files are ordinary versioned repository data and can be produced by a read-only analysis attempt, reviewed, and committed through the normal workflow. No separate memory service or semantic index is required. Exporting selected project inputs retains knowledge paths; referenced files must accompany that export. Automatic analysis scheduling and model-quality evaluation remain separate from this data contract.

Limits: eight snapshot files per input selection, 128 KiB per snapshot, 128 dependencies and 64 claims per snapshot, 16 locations per claim, and 256 distinct dependency reads across a frozen run. Duplicate/unsupported JSON fields, unsafe paths, untracked locations, out-of-range locations in unchanged files, and nonintersecting snapshot scopes reject loading. Dependency observations are cached within one freeze, not across source revisions.

Local Git/PostgreSQL E2E covers matching dependencies, unrelated edits, changed interfaces, scoped mode detection, durable replay, worker delivery, and exclusion of stale candidate claims. These checks do not certify a model's behavioral analysis or a live AX environment.
