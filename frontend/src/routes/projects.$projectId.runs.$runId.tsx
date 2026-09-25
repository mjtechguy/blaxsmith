import { createContext, useContext, useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, Navigate } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowRight, Check, GitCommitHorizontal, RefreshCw, Terminal, TerminalSquare } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { openLiveStream } from "../live";
import { DataTable } from "../data-table";
import type { RunTask } from "../gen/blaxsmith/api/v1/workflow_pb";
import { useGatewayEnabled } from "../gateway";
import { DetailLayout } from "../layouts";
import { isMissing, NotFoundPage } from "../page";
import { RunCostTab, StageCostChip, useRunCost } from "../run-cost";
import { Card, CopyValue, Disclosure, sentence, ShowMore, StatePanel, Timestamp, type TabSpec } from "../ui";
import { Inbox, interactionsKey, StagePanel, useInteractions } from "../run-live";
import { needsYou, runStatus, stageStatus, type AgentStatus } from "../agent-view";
import { StatusPill, useAttentionTitle } from "../work-log";
import { PacedChip } from "../paced-chip";
import { appendRunEvent, decideReview, eventsAfter, getCurrentReview, getRun, listCommandExits, listRunTasks, liveEventsUrl, parseLiveEvent, recoverRunEventBatch, type RunEventPages } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/runs/$runId")({
  component: RunDetail,
  validateSearch: (search: Record<string, unknown>): { stage?: string; tab?: string } => ({
    ...(typeof search.stage === "string" ? { stage: search.stage } : {}),
    ...(typeof search.tab === "string" && ["stages", "review", "cost"].includes(search.tab) ? { tab: search.tab } : {}),
  }),
});

const taskFeatures = tableFeatures({});
const activityLabels: Record<string, string> = {
  "run.created": "Run created",
  "run.graph_sealed": "Execution plan frozen",
  "run.cancel_requested": "Cancellation requested",
  "run.cancelled": "Run cancelled",
  "task.created": "Stage created",
  "attempt.reserved": "Worker attempt reserved",
  "attempt.starting": "Worker starting",
  "attempt.started": "Worker started",
  "attempt.runtime_bound": "Coding runtime connected",
  "attempt.unknown": "Worker state is uncertain",
  "attempt.result": "Worker result recorded",
  "attempt.stopped": "Worker attempt stopped",
  "attempt.command_exited": "Tool command exited",
  "review.presented": "Evidence presented for human review",
  "review.superseded": "Review package superseded",
  "review.approved": "Review approved",
  "review.changes_requested": "Changes requested",
  "interaction.opened": "Agent asked a question",
  "interaction.answered": "Question answered",
  "attempt.progress": "Agent progress",
  "attempt.control": "Terminal control changed",
};

// Stage status pills for the execution plan rows (docs/interactive-sessions.md, status rollup).
const StageStatuses = createContext<Map<string, AgentStatus | null>>(new Map());
function StageStatusCell({ taskKey }: { taskKey: string }) {
  return <StatusPill status={useContext(StageStatuses).get(taskKey) ?? null} />;
}

function activityLabel(kind: string) {
  return activityLabels[kind] ?? sentence(kind.split(".").map((part) => part.replaceAll("_", " ")).join(" · "));
}

const taskColumns: ColumnDef<typeof taskFeatures, RunTask>[] = [
  { id: "stage", accessorKey: "key", header: "Stage", cell: ({ row }) => <span className="task-stage"><strong>{row.original.key}</strong><small title={row.original.id}>Task {row.original.id.slice(0, 8)}</small></span> },
  { id: "profile", header: "Selected runtime", cell: ({ row }) => row.original.kind === "human_review" ? "Human review"
    : <span className="task-stage"><strong>{row.original.harness}</strong><small>{[row.original.kind.replaceAll("_", " "), row.original.model, row.original.effort,
      row.original.loopWith ? `loop ${row.original.loopCycles}/${row.original.maxCycles} with ${row.original.loopWith}` : ""].filter(Boolean).join(" · ")}</small></span> },
  { id: "inputs", header: "Frozen skills and instructions", cell: ({ row }) => {
    const task = row.original;
    const files = [
      ...task.instructionFiles.map((file) => ({ ...file, kind: "Instruction" })),
      ...task.skillFiles.map((file) => ({ ...file, kind: "Skill" })),
    ];
    if (!files.length) return "—";
    return <Disclosure summary={`${task.skillFiles.length} skills · ${task.instructionFiles.length} instructions`}>
      <ul className="frozen-files">{files.map((file) => <li key={`${file.kind}:${file.path}`}><strong>{file.kind}</strong> <code>{file.path}</code>
        <CopyValue value={file.sha256} label="SHA-256" chars={12} /></li>)}</ul>
    </Disclosure>;
  } },
  { id: "depends", accessorKey: "dependsOn", header: "Depends on", cell: ({ row }) => row.original.dependsOn.length ? row.original.dependsOn.join(", ") : "Start" },
  { id: "status", header: "Status", cell: ({ row }) => <StageStatusCell taskKey={row.original.key} /> },
  { id: "state", accessorKey: "state", header: "State", cell: ({ row }) => <span className={`state-badge state-${row.original.state}`}>{sentence(row.original.state)}</span> },
  { id: "attempts", accessorKey: "generation", header: "Attempts", cell: ({ row }) => `${row.original.generation.toString()} of ${row.original.maxAttempts}` },
  { id: "active", accessorKey: "activeAttemptId", header: "Active attempt", cell: ({ row }) => <CopyValue value={row.original.activeAttemptId} label="Attempt ID" /> },
  { id: "live", header: "Live", cell: ({ row }) => <Link from={Route.fullPath} to={Route.fullPath} search={{ stage: row.original.key, tab: "stages" }} className="text-action" aria-label={`Open stage ${row.original.key}`}>
    <TerminalSquare size={14} aria-hidden="true" /> {row.original.activeAttemptId ? "Watch" : "Open"}</Link> },
];

function RunDetail() {
  const { projectId, runId } = Route.useParams();
  const search = Route.useSearch();
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
  const taskNames = useMemo(() => new Map((tasks.data?.tasks || []).map((task) => [task.id, task.key])), [tasks.data]);
  const latestDelivered = useRef(0n);
  const recoverRef = useRef<(() => Promise<void>) | null>(null);
  const [eventStream, setEventStream] = useState<"connecting" | "connected" | "reconnecting">("connecting");
  const interactions = useInteractions(runId, scope, !!scope && run.data?.run?.projectId === projectId);
  const selectedKey = search.stage ?? tasks.data?.tasks.find((task) => task.activeAttemptId)?.key;
  const selectedTask = tasks.data?.tasks.find((task) => task.key === selectedKey);
  const mayOperate = session.data?.role === "owner" || session.data?.role === "admin" || session.data?.role === "member";
  const review = useQuery({ queryKey: reviewKey, queryFn: ({ signal }) => getCurrentReview(runId, signal), enabled: !!scope && run.data?.run?.projectId === projectId });
  const reviewWaiting = !!review.data && !review.data.decision;
  const statuses = useMemo(() => new Map((tasks.data?.tasks || []).map((task) =>
    [task.key, stageStatus(task, interactions.data ?? [], tasks.data!.tasks, reviewWaiting)] as const)), [tasks.data, interactions.data, reviewWaiting]);
  const gatewayEnabled = useGatewayEnabled();
  const runCost = useRunCost(scope, runId, gatewayEnabled && run.data?.run?.projectId === projectId);
  const overall = run.data?.run ? runStatus(run.data.run.state, tasks.data?.tasks || [], interactions.data ?? [], reviewWaiting) : null;
  useAttentionTitle([...statuses.values()].filter(needsYou).length);

  useEffect(() => {
    if (!scope || run.data?.run?.projectId !== projectId || !activity.isSuccess) return;
    const key = ["run-events", scope, runId];
    const cursor = () => queryClient.getQueryData<RunEventPages>(key)?.pages.at(-1)?.nextAfterId ?? 0n;
    latestDelivered.current = cursor();
    let closed = false;
    let recovering = false;
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
    const onMessage = (data: string) => {
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
        if (event.kind.startsWith("interaction.")) void queryClient.invalidateQueries({ queryKey: interactionsKey(scope, runId) });
        if (event.kind === "attempt.command_exited") void queryClient.invalidateQueries({ queryKey: ["run-command-exits", scope, runId] });
      } catch { void recover(); }
    };
    // Reconnects after a restart (renewing the session first) and replays
    // from the newest cursor; recover() fills any gap once it reopens.
    const stop = openLiveStream(() => liveEventsUrl(runId, cursor()), {
      open: () => void recover(), message: onMessage, state: setEventStream,
      sessionEnded: () => void queryClient.invalidateQueries({ queryKey: sessionQueryKey }),
    });
    return () => { closed = true; window.clearTimeout(retryRecovery); recoverRef.current = null; stop(); };
  }, [activity.isSuccess, projectId, queryClient, run.data?.run?.projectId, runId, scope]);

  useEffect(() => {
    if (activity.data?.pages.at(-1)?.nextAfterId && activity.data.pages.at(-1)!.nextAfterId < latestDelivered.current) {
      void recoverRef.current?.();
    }
  }, [activity.data]);

  if (run.data?.run && run.data.run.projectId !== projectId) {
    return <Navigate to="/projects/$projectId/runs/$runId" params={{ projectId: run.data.run.projectId, runId }} replace />;
  }

  const runData = run.data?.run;
  const tabs: TabSpec[] = [
    { id: "overview", label: "Overview" },
    { id: "stages", label: "Stages", count: tasks.data?.tasks.length },
    { id: "review", label: "Review" },
    { id: "cost", label: "Cost", hidden: !gatewayEnabled },
  ];
  const tab = search.tab === "cost" && !gatewayEnabled ? "overview" : search.tab ?? (search.stage ? "stages" : "overview");
  const openItems = (interactions.data ?? []).filter((item) => item.state === "open");
  const stageCounts = new Map<string, number>();
  for (const status of statuses.values()) if (status) stageCounts.set(status, (stageCounts.get(status) ?? 0) + 1);
  const refresh = () => { void run.refetch(); void tasks.refetch(); void activity.refetch(); void commandExits.refetch(); void queryClient.invalidateQueries({ queryKey: reviewKey }); };
  const tabLink = (target: string, label: string) => <Link from={Route.fullPath} to={Route.fullPath} search={{ tab: target }} className="text-action">{label} <ArrowRight size={13} aria-hidden="true" /></Link>;

  if (run.isPending) return <StatePanel kind="loading" title="Loading run" />;
  if (run.isError && isMissing(run.error)) return <NotFoundPage title="Run not found" back={{ to: `/projects/${projectId}/runs`, label: "Project runs" }}>
    This run does not exist or is not in your organization.</NotFoundPage>;
  if (run.isError || !runData) return <StatePanel kind="error" title="Run unavailable" retry={() => void run.refetch()}>This run could not be loaded.</StatePanel>;

  const activityCard = <section className="table-section" aria-labelledby="activity-heading">
    <div className="table-heading"><div><h2 id="activity-heading">Activity</h2><p>Recorded workflow events, newest first. The live stream reflects server events, not AX pod health.</p></div><span className="fetched-time" role="status" aria-live="polite">{eventStream === "connected" ? "Live stream connected" : eventStream === "reconnecting" ? "Reconnecting" : "Connecting"} · {events.length} events</span></div>
    {activity.isPending ? <div className="table-empty" role="status">Loading activity…</div> : null}
    {activity.isError ? <div className="table-empty" role="alert">Activity is unavailable. <button type="button" className="text-action" onClick={() => void activity.refetch()}>Try again</button></div> : null}
    {activity.data && events.length === 0 ? <div className="table-empty">No activity has been recorded yet.</div> : null}
    {events.length > 0 ? <ol className="event-list"><ShowMore items={events.slice().reverse()} initial={12} noun="older events" render={(event) => <li key={event.id.toString()}><span className="event-mark"><GitCommitHorizontal size={15} aria-hidden="true" /></span><div><strong>{activityLabel(event.kind)}</strong><small>{[event.taskId ? (taskNames.has(event.taskId) ? `Stage ${taskNames.get(event.taskId)}` : `Task ${event.taskId.slice(0, 8)}`) : "", event.attemptId ? `Attempt ${event.attemptId.slice(0, 8)}` : ""].filter(Boolean).join(" · ")}</small></div><Timestamp value={event.occurredAt} /></li>} /></ol> : null}
    {activity.hasNextPage ? <div className="table-footer"><span>More activity may be available</span><button type="button" className="secondary-button" disabled={activity.isFetchingNextPage} onClick={() => void activity.fetchNextPage()}>{activity.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
  </section>;

  return <DetailLayout back={{ href: `/projects/${projectId}/runs`, label: "Project runs" }} title={runData.launchKey}
    status={<><span className={`state-badge state-${runData.state}`}>{sentence(runData.state)}</span> <StatusPill status={overall} /></>}
    facts={[
      { label: "Source commit", value: <CopyValue value={runData.sourceCommit} label="Source commit" chars={12} /> },
      { label: "Created", value: <Timestamp value={runData.createdAt} /> },
      { label: "Stages", value: tasks.data ? `${tasks.data.tasks.filter((t) => t.state === "succeeded").length} of ${tasks.data.tasks.length} succeeded` : "—" },
      { label: "Open items", value: openItems.length },
      { label: "Run ID", value: <CopyValue value={runId} label="Run ID" /> },
    ]}
    actions={<button type="button" className="secondary-button" onClick={refresh}><RefreshCw size={15} aria-hidden="true" /> Refresh</button>}
    tabs={tabs} current={tab} tabsLabel="Run sections">
    {tab === "overview" ? <>
      {interactions.data ? <Inbox items={interactions.data} mayAnswer={mayOperate} scope={scope} runId={runId} projectId={projectId} /> : null}
      <div className="dash-grid">
        <Card title="Stages" className="dash-main" description={tasks.data ? `${tasks.data.tasks.length} frozen stages. Attempts count reservations, not verified results.` : "Loading stages…"} actions={tabLink("stages", "Open stages")}>
          {tasks.data ? <ul className="stage-strip card-body">{tasks.data.tasks.map((task) => <li key={task.id}>
            <Link from={Route.fullPath} to={Route.fullPath} search={{ tab: "stages", stage: task.key }} className="stage-chip"><strong>{task.key}</strong>
              <StatusPill status={statuses.get(task.key) ?? null} /><small>{task.state.replaceAll("_", " ")}</small><PacedChip reason={task.pacedReason} resetsAt={task.pacedResetsAt} />{gatewayEnabled ? <StageCostChip stage={task.key} stages={runCost.data?.stages} /> : null}</Link></li>)}</ul> : null}
          {tasks.isError ? <p className="card-body" role="alert">Stages are unavailable. <button type="button" className="text-action" onClick={() => void tasks.refetch()}>Try again</button></p> : null}
          {stageCounts.size ? <p className="card-note">{[...stageCounts].map(([status, count]) => `${count} ${status.replaceAll("_", " ")}`).join(" · ")}</p> : null}
        </Card>
        <Card title="Final review" className="dash-side" description={review.data ? review.data.decision ? `Decided: ${review.data.decision.action === "approve" ? "approved" : "changes requested"}.` : `Revision ${review.data.revision.toString()} is awaiting a decision.` : runData.state === "succeeded" ? "Execution finished; no verified package yet." : "Available after execution and verification."}
          actions={tabLink("review", "Open review")} />
      </div>
      {activityCard}
    </> : null}
    {tab === "stages" ? <>
      <section className="table-section" aria-labelledby="run-tasks-heading">
        <div className="table-heading"><div><h2 id="run-tasks-heading">Execution plan</h2><p>Frozen stages run after their dependencies. Open a stage to watch its work log or terminal.</p></div><span className="fetched-time">{tasks.data?.tasks.length ?? 0} stages</span></div>
        {tasks.isPending ? <div className="table-empty" role="status">Loading stages…</div> : null}
        {tasks.isError ? <div className="table-empty" role="alert">Execution plan is unavailable. <button type="button" className="text-action" onClick={() => void tasks.refetch()}>Try again</button></div> : null}
        {tasks.data ? <StageStatuses.Provider value={statuses}><DataTable table={taskTable} label="Run execution plan" empty="No stages have been frozen for this run." /></StageStatuses.Provider> : null}
      </section>
      {selectedTask && session.data ? <StagePanel key={selectedTask.id} task={selectedTask} principalId={session.data.principalId} mayControl={mayOperate}
        interactions={interactions.data ?? []} events={events} status={statuses.get(selectedTask.key) ?? null} scope={scope} runId={runId} /> : null}
      <section className="table-section" aria-labelledby="command-exits-heading">
        <div className="table-heading"><div><h2 id="command-exits-heading">Command observations</h2><p>Connector-signed exits are unverified. A zero exit does not verify artifacts or complete a task.</p></div><span className="fetched-time">{observations.length} observed</span></div>
        {commandExits.isPending ? <div className="table-empty" role="status">Loading command observations…</div> : null}
        {commandExits.isError ? <div className="table-empty" role="alert">Command observations are unavailable. <button type="button" className="text-action" onClick={() => void commandExits.refetch()}>Try again</button></div> : null}
        {commandExits.data && observations.length === 0 ? <div className="table-empty">No signed command exit has been recorded.</div> : null}
        {observations.length > 0 ? <ol className="event-list">{observations.map((observation) => <li key={observation.eventId.toString()}>
          <span className="event-mark"><Terminal size={15} aria-hidden="true" /></span>
          <div><strong>{observation.interrupted ? "Interrupted" : `Exited ${observation.exitCode}`}</strong>
            <small>{taskNames.has(observation.taskId) ? `Stage ${taskNames.get(observation.taskId)}` : `Task ${observation.taskId.slice(0, 8)}`} · Attempt {observation.attemptId.slice(0, 8)} · Unverified</small>
            <Disclosure summary="Receipt provenance" className="observation-provenance"><dl>
              <div><dt>Signer</dt><dd>{observation.signerId}</dd></div>
              <div><dt>Actor UID</dt><dd><code>{observation.actorUid}</code></dd></div>
              <div><dt>Receipt SHA-256</dt><dd><CopyValue value={observation.receiptSha256} label="Receipt SHA-256" chars={16} /></dd></div>
              {observation.signal ? <div><dt>Signal</dt><dd>{observation.signal}</dd></div> : null}
            </dl></Disclosure>
          </div>
          <Timestamp value={observation.receivedAt} />
        </li>)}</ol> : null}
        {commandExits.hasNextPage ? <div className="table-footer"><span>More observations may be available</span><button type="button" className="secondary-button" disabled={commandExits.isFetchingNextPage} onClick={() => void commandExits.fetchNextPage()}>{commandExits.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
      </section>
    </> : null}
    {tab === "review" ? <FinalReview runId={runId} scope={scope} state={runData.state} /> : null}
    {tab === "cost" ? <RunCostTab scope={scope} runId={runId} /> : null}
  </DetailLayout>;
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
    <div className="review-heading"><div><h2 id="review-heading">Human review</h2><p>A person makes the final decision on the current, verified package.</p></div>
      {current ? <span className={`state-badge ${current.decision?.action === "approve" ? "state-succeeded" : current.decision ? "state-failed" : "state-active"}`}>
        {current.decision?.action === "approve" ? "Approved" : current.decision ? "Changes requested" : "Awaiting decision"}</span> : null}</div>
    {review.isPending ? <p className="review-message" role="status">Checking for a review package…</p> : null}
    {review.isError ? <p className="review-message" role="alert">Review is unavailable. <button type="button" className="text-action" onClick={() => void review.refetch()}>Try again</button></p> : null}
    {review.isSuccess && !current ? <p className="review-message">{state === "succeeded" ? "Execution finished, but a verified evidence package has not been presented yet." : "Final review becomes available after execution and evidence verification."}</p> : null}
    {current ? <>
      <dl className="review-facts">
        <div><dt>Package</dt><dd>Revision {current.revision.toString()} · <Timestamp value={current.presentedAt} /></dd></div>
        <div><dt>Integrated commit</dt><dd><CopyValue value={current.integratedCommit} label="Integrated commit" chars={12} /></dd></div>
        <div><dt>Source commit</dt><dd><CopyValue value={current.sourceCommit} label="Source commit" chars={12} /></dd></div>
      </dl>
      <Disclosure summary="Provenance digests" className="review-provenance"><dl className="review-facts">
        <div><dt>Evidence SHA-256</dt><dd><CopyValue value={current.evidenceSha256} label="Evidence SHA-256" chars={16} /></dd></div>
        <div><dt>Verification policy SHA-256</dt><dd><CopyValue value={current.verificationSha256} label="Verification policy SHA-256" chars={16} /></dd></div>
        <div><dt>Recipe bundle SHA-256</dt><dd><CopyValue value={current.bundleSha256} label="Recipe bundle SHA-256" chars={16} /></dd></div>
      </dl></Disclosure>
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
