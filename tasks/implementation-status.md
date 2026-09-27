# Blaxsmith / Anvil implementation ledger

Scope: the two companion PRDs, including their qualification requirements. The user requested completion of all remaining work with limited unit testing and full E2E. Existing dirty-tree work is preserved. This ledger distinguishes implemented paths from remaining requirements; a passing local test does not complete a live-runtime requirement.

## Current execution

- Shared machine API: project-scoped delegated credentials, session-fenced mutations/revocation, explicit scopes, browser issuance UI, API/MCP parity — implemented and qualified locally through real HTTPS/PostgreSQL/browser/CLI E2E.
- MCP: official SDK stdio bridge to the machine API, scoped tools, durable IDs, instructions — implemented and qualified locally through the actual CLI subprocess. Sessionless HTTPS, service identities and remote OAuth for pre-registered public clients are implemented. OAuth includes browser consent, S256 PKCE, one-time codes, resource-bound credentials and shared revocation. Named third-party clients and richer registration/refresh capabilities remain open.
- E2E: real HTTPS app + PostgreSQL + browser + actual MCP subprocess, plus local Git/correction loop. `make e2e` is the reproducible entry point.
- Live AX environment: prior SSH access to `135.181.34.21` failed; a usable test environment/access path has been requested. Independent implementation continues.

## Platform PRD

| Phase | Existing implementation / evidence | Remaining acceptance work |
| --- | --- | --- |
| P00 baseline | Architecture, pinned AX reference, overlay inventory | Reconcile all overlays against upgrade target; complete release fixture inventory |
| P01 separation | Neutral recipe graph, explicit Guild validator, Anvil seed, frozen inputs | Full live independence and external conformance proof |
| P02 AX migration | Updated reference checkout; current qualified integration retained; 15-overlay port reconciled into a reproducible candidate at d0bc38b; candidate full Go/vet, real gRPC/Redis lifecycle checks, race checks and four Linux builds pass; see `integrations/ax/upgrade-reconciliation.md` | Blaxsmith connector migration, actor-side lifecycle ordering, live networking/fault matrix and new qualified pin |
| P03 shared operations | Shared workflow stores, durable goals/plans, external factory intake, opaque plans, frozen recipe association, preview, capabilities, machine entry point, immutable reported usage and accepted-run checkpoints | Metered usage/reservation contract and live external run/plan qualification |
| P04 identity | Delegated user and administrator-managed service credentials, project/scope bounds, rotation and expiry/revocation | Attempt-bound delegation, coordinator generations, complete parity matrix |
| P05 reliability | Existing launch/request keys, expected revisions, durable event reads | Uniform mutation contracts, explicit asynchronous operations, retention-gap snapshots, structured error details |
| P06 MCP | Stdio and sessionless HTTPS tools through authenticated API; scoped discovery and durable IDs; pre-registered OAuth clients with browser consent, resource/PKCE/code-replay fences and revocation | Named third-party client/extension qualification, optional CIMD/registration/refresh support, broader conformance |
| P07 tools | Existing harness adapters, approved extension inputs, shared preflight | Complete permission/capability negotiation; outbound MCP parity; qualify conditional ACP/A2A consumers |
| P08 delegation | Embedded extensions and factory provenance | Versioned descriptors, child admission/budget/ownership/cancellation, external coordinator recovery |
| P09 factories | Native Anvil and explicit optional Guild paths | Pinned live Guild qualification, managed delegation adapter, cross-factory handoff proof |
| P10 docs | API/MCP guide, generated machine-operation contract with E2E drift check, executable external intake quickstart with real HTTPS E2E, pinned Guild adaptation guide and versioned transport matrix | Live execution quickstarts, managed delegation example after implementation, named third-party client qualification |
| P11 operations | Existing dispatch/recovery/observability | Complete fault matrix, rollout/rollback qualification, resource reconciliation and measured integration outcomes |

## Anvil PRD

| Phase | Existing implementation / evidence | Remaining acceptance work |
| --- | --- | --- |
| P00 baseline | Native contracts and focused fixtures | Reconcile full qualification corpus and outcome measures |
| P01 independence | Native schemas, frozen provenance, example | Live native/Guild-independent execution qualification |
| P02 tunable quality | Required/advisory/off checks, editable categorized presets, immutable policy history, stale-save rejection, manual/policy acceptance, correction cycles | Full policy across interview/other phases; active-run policy amendments and invalidation UX |
| P03 inputs | Per-phase rules/skills, committed scoped inputs, structural conflict rejection, stack proposals, versioned agent definitions, preview/export and pinned worker context | Live guidance compliance; automated environment prerequisite qualification and organization-locked guidance layering |
| P04 understanding | Pinned baseline/mode evidence, Git-backed behavior snapshots, dependency freshness, stale-claim exclusion and candidate revalidation labels; planning checks | Automated scoped analysis/refresh, historical baseline comparison, empty-remote source bootstrap and live semantic qualification |
| P05 interview | Durable starter/follow-up questions, adaptive prompts, cited decisions/classified unknowns, visual editing and tunable readiness | Live interview-quality corpus and richer imported-spec reconciliation |
| P06 planning | Validated versioned plans, task packets, structural visual edits, downstream impact previews, coverage and version comparison, launch preview | Semantic handoff/impact validation and controlled invalidation across executing tasks |
| P07 models/resources | Granted project/personal catalog discovery and frozen per-phase account/model/harness/effort selection, phase guidance with dated provider positioning, runtime caps, shared goal run/attempt admission allowances and deadline, normalized harness usage/cost subtotals with unknown coverage | Broader model/benchmark coverage, trusted metering/price provenance, token/cost reservations and escalation |
| P08 serial execution | Native implement/verify/optional independent review, candidate branch, bounded repairs | Full live model/runtime qualification, broader failure handling and progress evidence |
| P09 thorough validation | Frozen project checks, separate review profiles, structured candidate-bound review reports, required/advisory correction behavior | Full app/E2E environment contract, deeper review-quality evaluation and contradiction resolution |
| P10 long goals | Durable goal context/plans/run associations; cross-run admission allowances; persisted pause/resume and cancellation with confirmed run termination; immutable accepted-run checkpoints with supersession status and explicit exact-commit source continuation across native/external factories; configurable required-check failure/progress stall limits with explicit recovery windows | Milestone controller decisions, accepted-task proof, broader failure classification and bounded automatic continuation |
| P11 parallel work | Existing platform stage dependencies | Workspace/write ownership, bounded task admission, integration/merge conflict policy and cancellation |
| P12 delivery/operations | Current review/evidence views, snapshot Markdown export, run-branch machinery | Complete requirement-level delivery proof, authorized provider delivery, evaluation corpus, live operations/qualification |

All task-level acceptance criteria remain in the source PRDs. Conditional integrations are not represented as implemented merely because a manifest or protocol exists. No live AX/provider completion, production deployment, merge, or broader roadmap completion is claimed by this ledger.

## Local qualification evidence

- Full Go suite on isolated PostgreSQL 18.6 schemas and `go vet ./...` pass through service identities, HTTPS MCP, goal control, account selection and reported usage. Accepted-run checkpoints also pass the full Go suite, vet and real browser E2E.
- Frontend conventions, auth (8) and workflow (53) checks pass. Production build/type checking passes through reported usage and checkpoint UI.
- Real HTTPS/browser E2E passes policy presets/stale drafts, structural plan edits/coverage/diffs/downstream impact, goal allowances/control, model advice/application/account pinning, external factory isolation, machine/service credential lifecycle, usage coverage, historical checkpoint display and delivery exports. Actual MCP subprocess and local Git correction/evidence fixtures pass in the same `make e2e` run.
- Additional integration checks cover personal-account isolation, explicit company-account selection, missing/reassigned account pins failing without fallback, pause after replacement, idempotent control, stale control rejection and requested versus confirmed cancellation.
- Tests use an isolated host PostgreSQL instance because the existing Docker VM ran out of disk and stopped its database. No existing Docker volumes were deleted and no application deployment settings were changed.
- Most checks extend integrated E2E paths; one small usage parser boundary check covers numeric normalization and hostile/missing counters. Live AX/provider qualification remains outstanding.

- AX candidate overlay builds from a clean archive of d0bc38b with recorded hashes. It preserves bootstrap boundaries and adds atomic immutable creation, task-owned deny-default egress, lifecycle status generations and checked workspace inline writes. No live Substrate qualification or default-pin change is claimed.
- The external factory quickstart is exercised as a real executable in `make e2e`, including identical retry, changed-request rejection, insufficient permissions and absence of unintended runs.

- Remote MCP OAuth uses the same domain authority and revocation fences. Real HTTPS/PostgreSQL checks cover metadata, redirect/scopes/CSRF, PKCE/client/resource binding, expiry/reuse, endpoint isolation and rate limiting; browser consent and revocation are included in `make e2e`. Full Go and vet pass through the OAuth implementation.

- Checkpoint continuation passes the real local Git/PostgreSQL correction flow: an accepted pushed candidate starts another run, which produces its own evidence and acceptance; source lineage is frozen, exported and checked against supersession and configuration changes. Live provider fetch/AX execution is not claimed.

- Goal stall limits use persisted platform check/acceptance evidence, stop shared run/attempt admission, and expose an explicit browser recovery window without resetting count/usage history. Real local Git/PostgreSQL E2E covers repeated failures, store replacement, elapsed progress limits and recovery; automatic semantic failure classification is not claimed.

## Latest verification (2026-09-26)

- `make e2e`: passed after checkpoint continuation, stall limits and recovery UI. Includes the actual HTTPS app, isolated PostgreSQL, Chrome/mobile views, MCP CLI/HTTP/OAuth and the local Git correction/continuation fixture.
- `go test -p 2 ./...`, `go vet ./...`, frontend production build/type checks, UI conventions, `buf lint` and `git diff --check`: passed.
- Mobile recovery controls and OAuth consent were inspected from actual browser screenshots. No new unit-only suite was added for continuation or stall admission; their boundaries are exercised through integrated flows.
- No deployment, production migration, qualified AX pin change, merge or commit was performed. The complete PRDs remain open for the remaining work listed above.
