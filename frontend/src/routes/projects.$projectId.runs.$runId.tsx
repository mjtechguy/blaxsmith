import { useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, Navigate } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, Check, GitCommitHorizontal, RefreshCw, Terminal } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { RunTask } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { appendRunEvent, decideReview, eventsAfter, getCurrentReview, getRun, listCommandExits, listRunTasks, liveEventsUrl, parseLiveEvent, recoverRunEventBatch, type RunEventPages } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/runs/$runId")({ component: RunDetail });

const taskFeatures = tableFeatures({});
const taskColumns: ColumnDef<typeof taskFeatures, RunTask>[] = [
  { id: "stage", accessorKey: "key", header: "Stage", cell: ({ row }) => <span className="task-stage"><strong>{row.original.key}</strong><small title={row.original.id}>Task {row.original.id.slice(0, 8)}</small></span> },
  { id: "depends", accessorKey: "dependsOn", header: "Depends on", cell: ({ row }) => row.original.dependsOn.length ? row.original.dependsOn.join(", ") : "Start" },
  { id: "state", accessorKey: "state", header: "State", cell: ({ row }) => <span className={`state-badge state-${row.original.state}`}>{row.original.state.replaceAll("_", " ")}</span> },
  { id: "attempts", accessorKey: "generation", header: "Attempts", cell: ({ row }) => `${row.original.generation.toString()} of ${row.original.maxAttempts}` },
  { id: "active", accessorKey: "activeAttemptId", header: "Active attempt", cell: ({ row }) => row.original.activeAttemptId ? <code title={row.original.activeAttemptId}>{row.original.activeAttemptId.slice(0, 8)}</code> : "—" },
];

function RunDetail() {
  const { projectId, runId } = Route.useParams();
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const scope = session.data ? `${session.data.organizationId}:${session.data.principalId}` : "";
  const runKey = ["run", scope, runId];
  const eventKey = ["run-events", scope, runId];
  const reviewKey = ["run-review", scope, runId];
  const exitKey = ["run-command-exits", scope, runId];
  const taskKey = ["run-tasks", scope, runId];
  const run = useQuery({ queryKey: runKey, queryFn: ({ signal }) => getRun(runId, signal), enabled: !!scope });
  const tasks = useQuery({ queryKey: taskKey, queryFn: ({ signal }) => listRunTasks(runId, signal), enabled: !!scope && run.data?.run?.projectId === projectId });
  const taskTable = useTable({ features: taskFeatures, data: tasks.data?.tasks || [], columns: taskColumns, getRowId: (task) => task.id });
  const activity = useInfiniteQuery({
    queryKey: eventKey, enabled: !!scope && run.data?.run?.projectId === projectId, initialPageParam: 0n,
    queryFn: ({ pageParam, signal }) => eventsAfter(runId, pageParam, signal),
    getNextPageParam: (page) => page.events.length === 100 ? page.nextAfterId : undefined,
  });
  const commandExits = useInfiniteQuery({
    queryKey: exitKey, enabled: !!scope && run.data?.run?.projectId === projectId, initialPageParam: 0n,
    queryFn: ({ pageParam, signal }) => listCommandExits(runId, pageParam, signal),
    getNextPageParam: (page) => page.observations.length === 50 ? page.nextAfterEventId : undefined,
  });
  const observations = useMemo(() => commandExits.data?.pages.flatMap((page) => page.observations) || [], [commandExits.data]);
  const events = useMemo(() => activity.data?.pages.flatMap((page) => page.events) || [], [activity.data]);
  const latestDelivered = useRef(0n);
  const recoverRef = useRef<(() => Promise<void>) | null>(null);

  useEffect(() => {
    if (!scope || run.data?.run?.projectId !== projectId || !activity.isSuccess) return;
    const key = ["run-events", scope, runId];
    const cursor = () => queryClient.getQueryData<RunEventPages>(key)?.pages.at(-1)?.nextAfterId ?? 0n;
    latestDelivered.current = cursor();
    let closed = false;
    let recovering = false;
    let checkedSession = false;
    let retryRecovery: number | undefined;
    const recover = async () => {
      if (closed || recovering) return;
      window.clearTimeout(retryRecovery);
      retryRecovery = undefined;
      recovering = true;
      let more = false;
      const before = cursor();
      try {
        more = await recoverRunEventBatch((after) => eventsAfter(runId, after), cursor,
          (event) => { if (!closed) queryClient.setQueryData<RunEventPages>(key, (old) => appendRunEvent(old, event).data); });
        if (!closed && cursor() !== before) void queryClient.invalidateQueries({ queryKey: ["run-tasks", scope, runId] });
      } catch {
        if (!closed) retryRecovery = window.setTimeout(() => void recover(), 5_000);
      } finally {
        recovering = false;
        if (!closed && retryRecovery === undefined && cursor() < latestDelivered.current) {
          retryRecovery = window.setTimeout(() => void recover(), more ? 0 : 5_000);
        }
      }
    };
    recoverRef.current = recover;
    const source = new EventSource(liveEventsUrl(runId, cursor()));
    source.onopen = () => { checkedSession = false; void recover(); };
    source.onmessage = ({ data }) => {
      try {
        const event = parseLiveEvent(data, runId);
        latestDelivered.current = event.id > latestDelivered.current ? event.id : latestDelivered.current;
        let gap = false;
        queryClient.setQueryData<RunEventPages>(key, (old) => {
          const merged = appendRunEvent(old, event);
          gap = merged.gap;
          return merged.data;
        });
        if (gap) void recover();
        if (event.kind.startsWith("run.")) void queryClient.invalidateQueries({ queryKey: ["run", scope, runId] });
        if (event.kind.startsWith("task.") || event.kind.startsWith("attempt.")) void queryClient.invalidateQueries({ queryKey: ["run-tasks", scope, runId] });
        if (event.kind.startsWith("review.")) void queryClient.invalidateQueries({ queryKey: ["run-review", scope, runId] });
        if (event.kind === "attempt.command_exited") void queryClient.invalidateQueries({ queryKey: ["run-command-exits", scope, runId] });
      } catch { void recover(); }
    };
    source.onerror = () => {
      if (!checkedSession) { checkedSession = true; void queryClient.invalidateQueries({ queryKey: sessionQueryKey }); }
    };
    return () => { closed = true; window.clearTimeout(retryRecovery); recoverRef.current = null; source.close(); };
  }, [activity.isSuccess, projectId, queryClient, run.data?.run?.projectId, runId, scope]);

  useEffect(() => {
    if (activity.data?.pages.at(-1)?.nextAfterId && activity.data.pages.at(-1)!.nextAfterId < latestDelivered.current) {
      void recoverRef.current?.();
    }
  }, [activity.data]);

  if (run.data?.run && run.data.run.projectId !== projectId) {
    return <Navigate to="/projects/$projectId/runs/$runId" params={{ projectId: run.data.run.projectId, runId }} replace />;
  }

  return <PageShell>
    <PageHeader eyebrow="Project / Run" title={run.data?.run?.launchKey || "Run"} description="Execution record and durable event history."
      actions={<button type="button" className="secondary-button" onClick={() => { void run.refetch(); void tasks.refetch(); void activity.refetch(); void commandExits.refetch(); void queryClient.invalidateQueries({ queryKey: reviewKey }); }}><RefreshCw size={15} aria-hidden="true" /> Refresh</button>} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Project runs</Link>
    {run.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading run</h2></div> : null}
    {run.isError ? <div className="state-panel" role="alert"><h2>Run unavailable</h2><p>This run could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void run.refetch()}>Try again</button></div> : null}
    {run.data?.run ? <div className="summary-grid">
      <section className="summary-card"><span className="summary-label">State</span><strong className="summary-value"><span className={`state-badge state-${run.data.run.state}`}>{run.data.run.state.replaceAll("_", " ")}</span></strong><span className="summary-meta">Engineering execution</span></section>
      <section className="summary-card"><span className="summary-label">Source commit</span><strong className="summary-value mono" title={run.data.run.sourceCommit}>{run.data.run.sourceCommit.slice(0, 12)}</strong><span className="summary-meta">Pinned at launch</span></section>
      <section className="summary-card"><span className="summary-label">Created</span><strong className="summary-value"><time dateTime={run.data.run.createdAt}>{new Date(run.data.run.createdAt).toLocaleDateString()}</time></strong><span className="summary-meta">{new Date(run.data.run.createdAt).toLocaleTimeString()}</span></section>
    </div> : null}
    {run.data?.run ? <section className="table-section" aria-labelledby="run-tasks-heading">
      <div className="table-heading"><div><h2 id="run-tasks-heading">Execution plan</h2><p>Frozen stages run after their dependencies. Attempts count reservations, not verified results.</p></div><span className="fetched-time">{tasks.data?.tasks.length ?? 0} stages</span></div>
      {tasks.isPending ? <div className="table-empty" role="status">Loading stages…</div> : null}
      {tasks.isError ? <div className="table-empty" role="alert">Execution plan is unavailable. <button type="button" className="text-action" onClick={() => void tasks.refetch()}>Try again</button></div> : null}
      {tasks.data ? <DataTable table={taskTable} label="Run execution plan" empty="No stages have been frozen for this run." /> : null}
    </section> : null}
    {run.data?.run ? <FinalReview runId={runId} scope={scope} state={run.data.run.state} /> : null}
    {run.data?.run ? <section className="table-section" aria-labelledby="command-exits-heading">
      <div className="table-heading"><div><h2 id="command-exits-heading">Command observations</h2><p>Connector-signed exits are unverified. A zero exit does not verify artifacts or complete a task.</p></div><span className="fetched-time">{observations.length} observed</span></div>
      {commandExits.isPending ? <div className="table-empty" role="status">Loading command observations…</div> : null}
      {commandExits.isError ? <div className="table-empty" role="alert">Command observations are unavailable. <button type="button" className="text-action" onClick={() => void commandExits.refetch()}>Try again</button></div> : null}
      {commandExits.data && observations.length === 0 ? <div className="table-empty">No signed command exit has been recorded.</div> : null}
      {observations.length > 0 ? <ol className="event-list">{observations.map((observation) => <li key={observation.eventId.toString()}>
        <span className="event-mark"><Terminal size={15} aria-hidden="true" /></span>
        <div><strong>{observation.interrupted ? "Interrupted" : `Exited ${observation.exitCode}`}</strong>
          <small>Task {observation.taskId.slice(0, 8)} · Attempt {observation.attemptId.slice(0, 8)} · Unverified</small>
          <details className="observation-provenance"><summary>Receipt provenance</summary><dl>
            <div><dt>Signer</dt><dd>{observation.signerId}</dd></div>
            <div><dt>Actor UID</dt><dd><code>{observation.actorUid}</code></dd></div>
            <div><dt>Receipt SHA-256</dt><dd><code>{observation.receiptSha256}</code></dd></div>
            {observation.signal ? <div><dt>Signal</dt><dd>{observation.signal}</dd></div> : null}
          </dl></details>
        </div>
        <time dateTime={observation.receivedAt}>{new Date(observation.receivedAt).toLocaleString()}</time>
      </li>)}</ol> : null}
      {commandExits.hasNextPage ? <div className="table-footer"><span>More observations may be available</span><button type="button" className="secondary-button" disabled={commandExits.isFetchingNextPage} onClick={() => void commandExits.fetchNextPage()}>{commandExits.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
    </section> : null}
    <section className="table-section" aria-labelledby="activity-heading">
      <div className="table-heading"><div><h2 id="activity-heading">Activity</h2><p>Committed events update live and replay after reconnecting.</p></div><span className="fetched-time">{events.length} events</span></div>
      {run.data?.run && activity.isPending ? <div className="table-empty" role="status">Loading activity…</div> : null}
      {activity.isError ? <div className="table-empty" role="alert">Activity is unavailable. <button type="button" className="text-action" onClick={() => void activity.refetch()}>Try again</button></div> : null}
      {activity.data && events.length === 0 ? <div className="table-empty">No activity has been recorded yet.</div> : null}
      {events.length > 0 ? <ol className="event-list">{events.map((event) => <li key={event.id.toString()}><span className="event-mark"><GitCommitHorizontal size={15} aria-hidden="true" /></span><div><strong>{event.kind.replaceAll(".", " · ").replaceAll("_", " ")}</strong><small>{event.taskId ? `Task ${event.taskId.slice(0, 8)} · ` : ""}{event.attemptId ? `Attempt ${event.attemptId.slice(0, 8)}` : ""}</small></div><time dateTime={event.occurredAt}>{new Date(event.occurredAt).toLocaleString()}</time></li>)}</ol> : null}
      {activity.hasNextPage ? <div className="table-footer"><span>More activity may be available</span><button type="button" className="secondary-button" disabled={activity.isFetchingNextPage} onClick={() => void activity.fetchNextPage()}>{activity.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
    </section>
  </PageShell>;
}

type ReviewAction = "approve" | "request_changes";

function FinalReview({ runId, scope, state }: { runId: string; scope: string; state: string }) {
  const queryClient = useQueryClient();
  const dialog = useRef<HTMLDialogElement>(null);
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const review = useQuery({ queryKey: ["run-review", scope, runId], queryFn: ({ signal }) => getCurrentReview(runId, signal) });
  const [confirmation, setConfirmation] = useState<{ action: ReviewAction; packageId: string } | null>(null);
  const [feedback, setFeedback] = useState("");
  const [error, setError] = useState("");
  const decide = useMutation({
    mutationFn: ({ action, packageId }: { action: ReviewAction; packageId: string }) => decideReview(runId, packageId, action, action === "request_changes" ? feedback : ""),
    onSuccess: async () => {
      setConfirmation(null);
      setFeedback("");
      setError("");
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["run-review", scope, runId] }),
        queryClient.invalidateQueries({ queryKey: ["run-events", scope, runId] }),
      ]);
    },
    onError: async (cause) => {
      const code = ConnectError.from(cause).code;
      setError(code === Code.FailedPrecondition ? "This review package changed. Check the latest package before deciding again."
        : code === Code.PermissionDenied ? "Your session is not allowed to make this decision."
          : "The decision could not be saved. Please try again.");
      await queryClient.invalidateQueries({ queryKey: ["run-review", scope, runId] });
    },
  });
  const current = review.data;
  const mayDecide = session.data?.role === "owner" || session.data?.role === "admin";
  const confirmCurrent = confirmation?.packageId === current?.id;
  const feedbackLength = Array.from(feedback.trim()).length;

  useEffect(() => {
    if (confirmation) dialog.current?.showModal();
    else dialog.current?.close();
  }, [confirmation]);

  return <section className="review-card" aria-labelledby="review-heading">
    <div className="review-heading"><div><p className="eyebrow">Final gate</p><h2 id="review-heading">Human review</h2><p>A person makes the final decision on the current, verified package.</p></div>
      {current ? <span className={`state-badge ${current.decision?.action === "approve" ? "state-succeeded" : current.decision ? "state-failed" : "state-active"}`}>
        {current.decision?.action === "approve" ? "Approved" : current.decision ? "Changes requested" : "Awaiting decision"}</span> : null}</div>
    {review.isPending ? <p className="review-message" role="status">Checking for a review package…</p> : null}
    {review.isError ? <p className="review-message" role="alert">Review is unavailable. <button type="button" className="text-action" onClick={() => void review.refetch()}>Try again</button></p> : null}
    {review.isSuccess && !current ? <p className="review-message">{state === "succeeded" ? "Execution finished, but a verified evidence package has not been presented yet." : "Final review becomes available after execution and evidence verification."}</p> : null}
    {current ? <>
      <dl className="review-facts">
        <div><dt>Package</dt><dd>Revision {current.revision.toString()} · <time dateTime={current.presentedAt}>{new Date(current.presentedAt).toLocaleString()}</time></dd></div>
        <div><dt>Integrated commit</dt><dd><code>{current.integratedCommit}</code></dd></div>
        <div><dt>Source commit</dt><dd><code>{current.sourceCommit}</code></dd></div>
        <div><dt>Evidence SHA-256</dt><dd><code>{current.evidenceSha256}</code></dd></div>
        <div><dt>Verification policy SHA-256</dt><dd><code>{current.verificationSha256}</code></dd></div>
        <div><dt>Recipe bundle SHA-256</dt><dd><code>{current.bundleSha256}</code></dd></div>
      </dl>
      {current.decision ? <div className="review-decision">
        <p><Check size={16} aria-hidden="true" /> {current.decision.action === "approve" ? "Approved" : "Changes requested"} by {current.decision.principalId === session.data?.principalId ? "you" : `principal ${current.decision.principalId}`} on <time dateTime={current.decision.decidedAt}>{new Date(current.decision.decidedAt).toLocaleString()}</time>.</p>
        {current.decision.feedback ? <p className="review-feedback">{current.decision.feedback}</p> : null}
      </div> : null}
      {!current.decision && mayDecide ? <div className="review-actions">
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <button type="button" className="secondary-button" disabled={decide.isPending} onClick={() => { setError(""); setFeedback(""); setConfirmation({ action: "request_changes", packageId: current.id }); }}>Request changes</button>
        <button type="button" className="primary-button" disabled={decide.isPending} onClick={() => { setError(""); setFeedback(""); setConfirmation({ action: "approve", packageId: current.id }); }}>Approve package</button>
      </div> : null}
      <dialog ref={dialog} className="review-confirm" aria-labelledby={`review-confirm-title-${runId}`}
        onCancel={(event) => { if (decide.isPending) event.preventDefault(); }}
        onClose={() => { setConfirmation(null); setFeedback(""); }}>
        {confirmation ? <>
          <h3 id={`review-confirm-title-${runId}`}>{confirmation.action === "approve" ? "Approve package" : "Request changes"}</h3>
          <p>{confirmCurrent && current ? `Confirm ${confirmation.action === "approve" ? "approval" : "request for changes"} for revision ${current.revision.toString()}?` : "The review package changed. Choose an action for the current revision."}</p>
          {confirmation.action === "request_changes" && confirmCurrent ? <div className="review-feedback-input">
            <label htmlFor={`review-feedback-${runId}`}>What needs to change?</label>
            <textarea id={`review-feedback-${runId}`} value={feedback} maxLength={4000} rows={4} onChange={(event) => setFeedback(event.target.value)} placeholder="Give the team specific corrections to make." />
            <small>10–4,000 characters. Be specific enough to guide the next pass.</small>
          </div> : null}
          <div className="review-confirm-actions">
            <button type="button" className="secondary-button" disabled={decide.isPending} onClick={() => { setConfirmation(null); setFeedback(""); }}>Cancel</button>
            <button type="button" className="primary-button" disabled={!confirmCurrent || decide.isPending || (confirmation.action === "request_changes" && feedbackLength < 10)} onClick={() => { setError(""); decide.mutate(confirmation); }}>{decide.isPending ? "Saving…" : "Confirm decision"}</button>
          </div>
        </> : null}
      </dialog>
      {!current.decision && !mayDecide && session.data ? <p className="review-message">Only organization owners and admins can make the final decision.</p> : null}
    </> : null}
  </section>;
}
