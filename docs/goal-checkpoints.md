# Accepted goal checkpoints

A checkpoint names an already accepted code-producing run and retains its exact goal context, saved plan version, acceptance package and candidate revision. Use it to identify completed milestones before revising the plan or attempting later work. It does not certify individual task/requirement completion, automatically launch continuation, merge code, or mark a different plan version complete.

In the goal workspace, **Accepted checkpoints → Record an accepted checkpoint** offers recent completed runs associated with saved plans. The browser displays the observed accepted candidate. The server rechecks the exact package and current goal revision before recording it. A pending, rejected, superseded, planning-only, failed or unrelated run cannot create a new checkpoint. Organization administrators and the goal's owner can record; ordinary readers can inspect the retained history.

`RecordGoalCheckpoint` takes `goalId`, `runId`, `packageId`, `title`, `requestKey`, and `expectedGoalRevision`. Machine clients need `goal.write` plus the same domain authority; this scope cannot grant acceptance. Matching retries return the retained receipt, even after later steering or review. Changed retries fail. One acceptance package can produce only one checkpoint per goal. Pausing a goal does not prevent recording already accepted work.

`ListGoalCheckpoints` needs `project.read`; page backward using `beforeSequence` and `nextBeforeSequence`. The immutable record survives future failed work, new plans, cancellation and process replacement. `currentAcceptance` is a live observation: false means the run is no longer succeeded under that exact accepted package. The browser labels these records **Historical acceptance · superseded**. Do not use that older candidate as automatically approved input for new work.

The ledger is capped at 10,000 checkpoints per goal and pages at 20. Writes serialize on the goal and run, fence the caller's live session, retain attribution and append an audit event. The PostgreSQL trigger forbids updates and deletions.

Qualification covers real HTTPS/PostgreSQL creation and replay, rejection before approval, changed replays, immutable storage, retained history after supersession, machine reads, and browser display/reload/disabled actions for rejected work. Signed-exit and runtime fixtures simulate AX; live AX and multi-milestone automatic continuation remain separate work. Granular phase selection, accepted-task proof and bounded controller continuation remain separate requirements.

## Continue from accepted code

In **Prepare implementation → Starting code**, choose a current accepted checkpoint or retain **Current project source**. The default does not silently pick the latest checkpoint. Preview shows the exact source commit and checkpoint ID. The current selected plan and project checks apply; all assigned tasks still run.

External factories use `goalContext.checkpointId` on ordinary `PreviewRun` and `LaunchRun`; Anvil uses `goalExecution.checkpointId`. Both share the same admission contract. The checkpoint must belong to the bound goal and the same configured repository. The server fetches its exact candidate SHA using current Git access, freezes fresh instructions from that commit, and requires the same SHA at admission. Unavailable commits fail without falling back to a branch. The checkpoint ID is part of the bundle digest and returned on `GoalRun` and preview.

Admission locks the parent run and rechecks its exact current accepted package, caller, goal/plan revision, configured repository/ref, verification policy, and normal run allowances. Workers receive the candidate SHA as their Git ref. The configured project ref remains separately recorded; changing it between preview preparation and admission rejects admission. New work writes its own run branch and needs its own checks and acceptance. Retries use the same launch key and preview digests; later supersession may require a new preview even for a repeated request.

Later supersession does not rewrite already admitted descendants or certify their results. Their lineage remains visible and they retain their own evidence/acceptance contract. Automatic descendant invalidation is not implemented.

The local Git/PostgreSQL E2E continues from a pushed, checked candidate into a fresh checkout, executes a second implementation/check cycle, verifies separate acceptance and exported lineage, and rejects wrong commits, changed previews, cross-goal/repository sources, changed configuration and superseded acceptance. Public provider Git fetch and live AX execution remain live qualification requirements.
