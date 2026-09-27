# Goal execution allowances

A goal owner or organization owner/admin can set maximum admitted runs, maximum admitted attempts, and an admission deadline in the goal workspace. Zero means no goal-specific count cap; an empty deadline means none. Existing per-stage retry/runtime limits still apply.

The allowance covers every run explicitly attached to the goal, including external factories, planning, implementation, checks, reviews, corrections, and interrupted attempts. Failed, cancelled and unresolved attempts remain counted. A transaction that rolls back reserves nothing. Retrying an identical launch key returns the existing run without consuming another run slot, including at the run limit.

Run admission, attempt reservation and allowance edits serialize on the goal row. Concurrent runs cannot consume the same remaining attempt slot. Exhausted attempt allowance prevents new runs and new attempts; a run limit only prevents new runs. Deadlines use the database clock. Existing workers are not killed by a smaller allowance or elapsed admission deadline; use [goal cancellation](goal-control.md) or the run's halt controls when that is intended.

Edits require a live browser session, the appropriate goal/organization authority and an expected allowance version. Stale edits fail with `aborted` and the browser preserves the draft. Each accepted edit appends an immutable history row and an audit event. The current version, setter and timestamp are available through `GetGoalAllowance`; complete historical rows are retained for administrative audit. Goal context/plan versions are not rewritten by allowance edits.

Machine API and MCP clients may read the allowance with `project.read`. They cannot modify it or increase their own delegated envelope. The browser API exposes `SetGoalAllowance` with `goal_id`, `expected_version`, `max_runs`, `max_attempts`, and optional RFC3339 `admit_until`. Preview reports exhausted allowance; admission repeats the check transactionally.

These are execution admission limits. They do not measure tokens, settled API charges, subscription allowance, or guarantee a provider-enforced spending cap. Token/cash reservations and provider usage reconciliation remain open work. Unassociated runs are outside this goal's allowance.

Local HTTPS/PostgreSQL/MCP/browser E2E covers edits, stale drafts, machine denial, current counts, immutable edit history, launch retries at the cap, concurrent cross-run attempt admission, unresolved attempts remaining counted, and expired admission deadlines.

## Stall boundaries and recovery

The same human-edited allowance offers `max_repeated_check_failures` (0 off, 1–100) and `no_progress_seconds` (0 off, 60–2,592,000). Both default to off. A reached boundary blocks new attached runs and attempts at the shared admission point, including after coordinator replacement. It does not terminate already admitted workers or claim provider spending control.

Repeated failures use consecutive **platform-recorded required-check** observations with the same policy digest, check ID, verdict, exit code, summary and output hash. Candidate SHA changes do not hide an unchanged failure. A different result breaks that check's streak. Advisory checks, worker prose and untrusted review claims do not count. The largest current streak is returned, capped at a lower bound of 101. Matching is exact; timestamp-heavy logs or semantically similar errors can differ. Generic model/infrastructure failure classification remains separate work.

The progress timer starts at the first admitted run. It advances for the first passing required-check observation for a given policy/check/candidate or the first accepted package for a candidate. Repeated passes on identical code, new run creation, logs, tokens and lease activity do not move it. Recorded progress is not goal completion or requirement-level proof. The clock continues through pauses and waiting for user input; choose a longer window, disable it, or explicitly authorize recovery when that behavior is desired.

`GetGoalAllowance` reports the current boundary reason, repeated count, progress timestamp and latest explicit recovery timestamp. The workspace shows these with choices to inspect evidence, change approach/model, narrow scope, wait or request help. No automatic account switch or gate relaxation occurs.

An authorized browser editor can select **Start a new stall recovery window**, sent as `reset_stall_window: true` with the current allowance version. The database records its start and immutable policy history. Failure matching then considers newer observations and the timer restarts if work already exists. This leaves all run/attempt counts, token usage, prior failures, deadline and frozen run inputs intact. A changed threshold alone does not discard the observed stall. Stale edits and machine attempts to reset still fail. Recovery is permission to try within the remaining limits, not a completion decision.

Real Git/PostgreSQL E2E exercises two identical failures, stopped new-run/attempt admission after store replacement, explicit recovery with retained counts, a second accepted run, and an elapsed progress window using an originally old fixture admission time. Browser E2E exercises persisted thresholds and stale-edit protection. No additional unit suite is required for these paths.
