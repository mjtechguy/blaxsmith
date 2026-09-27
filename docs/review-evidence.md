# Review evidence and independent checks

The Review tab separates **artifacts**, **agent checks**, and **platform checks**.
Content is fetched only when opened. Markdown uses the existing safe renderer;
JSON/ledgers render as text or bounded tables; PNG, JPEG, and GIF images are
validated before storage. SVG and HTML execution are not supported.

## Worker submissions

```sh
bx artifact --json '{"id":"report-1","path":"reports/result.md","kind":"report","title":"Implementation report","renderer":"markdown"}'
bx gate --json '{"id":"assay-1","check":"foundry-assay","verdict":"pass","summary":"All criteria covered","evidence":[{"path":"reports/result.md","sha256":"<artifact sha256>"}]}'
```

Artifacts must be regular files inside the checkout, at most 4 MiB each, with
at most 32 registrations per attempt. Registration returns the content hash;
completion collects the bytes before stopping the actor and rejects changed
content. This uses the existing guest bridge and PostgreSQL custody, without a
new artifact service. Reports larger than 128 KiB remain downloadable but are
not rendered inline.

Commit gate evidence before submitting it. `bx gate` waits for a durable
platform receipt; an accepted receipt means the submission was recorded, not
that its verdict passed. A check must be declared by that stage's frozen
extension. Declared checks named in `required_checks` must have a passing
verdict for the accepted stage revision; missing, failed, or stale gates block
acceptance. Other declared checks are informational. Reusing an id with changed
content is rejected. Agent gates never replace independent platform checks.

## Platform verification

Recipe stages with `kind: verify` now execute the project's frozen verification
commands directly, without launching the selected model harness. They reuse AX
workspace setup, the authenticated guest daemon, attempt ownership, and normal
stop/recovery handling. Model credentials are not released into these actors.
The current dispatcher still resolves the stage's approved runtime and model
binding during admission; this change does not remove those setup requirements.

The worker makes the candidate checkout read-only. Each check runs as a
different unprivileged UID with no capabilities, no new privileges, a clean
environment, and its own writable temporary home. Check commands cannot write
into the source tree: configure build/test output under `$TMPDIR` or another
permitted temporary location. Dependencies/tools must already be present in the
approved image. The existing sandbox CPU/memory and egress policy still apply.
Checks have at most two minutes each (or the shorter recipe timeout), bounded
64 KiB output, and the coordinator's overall deadline. Timeout, transport loss,
output overflow, and missing executables never count as passes.

Configure **Protected check paths** in Project → Verification to name test
scripts, directories, and configuration that must match the run's original
source revision. These are explicit paths, not an automatic discovery of every
transitive test dependency. Changes to a protected path block the check. The
platform also verifies the candidate commit and records the check's exit code,
output digest, policy digest, revision, and producing attempt.

An interrupted verification attempt is stopped and retried in a fresh actor;
it never reruns checks in a potentially contaminated sandbox. Existing attempts
that received model credentials or human control cannot become independent
verifiers. Completed observations are persisted before actor cleanup, so cleanup
can resume without rerunning checks. Failed checks enter the recipe's correction
loop; without a loop the stage blocks. Approval rechecks all required gates and
platform checks against current successful attempts and the integrated revision.
Old review packages without these observations cannot be approved.

Failed checks include the checked revision, all check statuses, command arguments,
and bounded output excerpts in the repair handoff. Long output keeps its beginning
and end; the complete captured bytes remain available in Review. Diagnostics are
untrusted context, not additional instructions. Each failed check receives a share
of the 16 KiB summary budget so an early noisy check cannot hide later failures.

A correction survives an interrupted worker: only one live attempt owns it, and
its next retry receives it again after the prior owner is confirmed stopped or
failed. The original attempt's handoff remains frozen. In the supported linear
code flow, the first implement stage repairs its latest accepted revision instead
of restarting from the run's original source. Downstream stages continue to use
their code-producing upstream revision.

Correction requests precede upstream summaries in the 64 KiB handoff. Upstream
context can be shortened with an explicit notice; requests are never silently
consumed after being cut out of the prompt. If the requests alone exceed the
limit, dispatch refuses that handoff instead of starting incomplete work.

## Permanent stage failures

When a stage exhausts retries, fails without a correction loop, or is halted,
its pending dependent stages become blocked. Propagation follows the entire
dependency graph, including shared downstream stages. Each newly blocked stage
gets a durable `task.blocked` event identifying the failed upstream stage; the
run activity view displays that cause. Repeated progress passes do not duplicate
these events.

Independent work continues. Live owners still require the normal stop proof,
and escalations still require a decision. Retryable failures do not block their
dependents. Once all work is terminal and no owner remains, the run becomes
failed instead of remaining active with unreachable pending stages.

## Scheduler responsiveness

Lease renewal, cancellation, and run progression have separate bounded passes
under the dispatch leader. A slow launch or verification does not consume their
deadlines. Launch and uncertain-launch recovery remain sequential so recovery
cannot mistake an active launch for an abandoned one. The leader connection is
checked independently; losing it cancels work and joins all background passes
before releasing the connection.

Cancellation bypasses verification and guest-result collection. An active
verification polls ownership every two seconds with a three-second database
probe deadline; loss of ownership interrupts the command and attempts process
cleanup. Cancellation then follows credential revocation and actor-gone proof.
A failed or slow cleanup keeps the run in its stopping state rather than claiming
that the worker is gone. Cancellation scans have their own cursor, so they do
not wait behind unrelated verification commands.

Model lease renewals remain pending in the database until the new expiry has
been written to the worker. Failed delivery or a lost acknowledgement replays
the same generation without extending its expiry again. Each retry rechecks
authorization and current ownership; cancelled runs receive no renewals. An
old acknowledgement cannot consume a newer renewal. OAuth credentials are
written before the expiry notice, and the lease retains the token-expiry cap.
Renewal scans use ordered pages of ten leases. The cursor advances past each
attempted delivery even when a guest consumes the pass deadline, while leases
not yet attempted remain ahead of it. The next pass resumes there; wrapping
at the end retries pending failures. A failed database query keeps the cursor.

## Rollout and validation

Apply migrations `0240_workflow_evidence.sql`, `0250_blocked_dependencies.sql`,
and `0260_lease_renewal_delivery.sql`
through the normal migration path,
and rebuild both the application and tool-worker images. The worker image now
explicitly includes util-linux for `setpriv`; older workers cannot run the new
commands. Drain old runs or let them retry using the new image. No deployment or
live provider run is implied by the local test results.

Run `make check`, the PostgreSQL-backed tests using
`BLAXSMITH_TEST_DATABASE_URL`, and `make web-check`. The development mock includes
artifact, platform-check, and previous-attempt gate examples on the Review tab.

`TestVerificationLinuxIsolation` exercises the actual checkout permissions and
`setpriv` boundary. Run it as root only in a disposable Linux container with
util-linux installed and `BLAXSMITH_TEST_VERIFICATION_ISOLATION=1`; normal host
test runs skip it. It rejects checkout/Git writes, permission changes, and writes
to another check’s home, while allowing the check’s own temporary files.

## Independent native review

Anvil implementation settings expose Off, Advisory and Required independent review. Enabled review adds a separate `review` stage after platform verification, with its own selected model, effort, rules and skills. The run's shared scope and time limits still apply. Off adds no reviewer. Required review can use the configured bounded correction loop; a correction reruns implementation, checks and review. Check and review loops each have their stated cap, and normal attempt limits still apply.

A review stage may declare `review_report: "anvil-review"` (any valid artifact ID is supported). This is a neutral recipe evidence contract, not a dependency on Anvil. The declared artifact must contain `blaxsmith.review/v1alpha1`, the exact candidate revision, a pass/fail verdict matching the attempt, a summary, bounded findings, requirement assessments and limitations. See `internal/evidence/review.go` for the complete typed contract. Unknown fields, oversized documents, invalid paths and contradictory candidate/verdict claims are rejected as review failures. Findings and assessments remain agent claims; the platform validates their structure and provenance, not their truth.

All review kinds must report the assigned candidate revision. A missing verdict becomes a recorded failure. Required failure blocks acceptance; advisory findings remain visible without blocking on that finding. Missing/invalid declared reports cannot satisfy required review. Structured report bytes remain immutable artifacts, available in Review and the delivery export's evidence index. They do not replace platform checks or authorize merge/deployment.

The integrated local execution fixture covers separate model selection, required/advisory behavior, missing verdict, missing report, contradictory report, stale candidate rejection and correction followed by a fresh review. It uses actual Git, PostgreSQL and signed completion receipts with simulated AX/harness observations. Live model review quality remains a separate qualification requirement.

Required declared review reports are revalidated at acceptance presentation and decision, including current task generation, candidate, verdict and frozen policy. A prematurely succeeded task cannot substitute for missing evidence.
