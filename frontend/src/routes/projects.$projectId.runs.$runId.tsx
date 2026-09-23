import { useMemo } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Navigate } from "@tanstack/react-router";
import { ArrowLeft, GitCommitHorizontal, RefreshCw } from "lucide-react";
import { PageHeader, PageShell } from "../page";
import { eventsAfter, getRun } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/runs/$runId")({ component: RunDetail });

function RunDetail() {
  const { projectId, runId } = Route.useParams();
  const run = useQuery({ queryKey: ["run", runId], queryFn: ({ signal }) => getRun(runId, signal), refetchInterval: 10_000 });
  const activity = useInfiniteQuery({
    queryKey: ["run-events", runId], enabled: run.data?.run?.projectId === projectId, initialPageParam: 0n,
    queryFn: ({ pageParam, signal }) => eventsAfter(runId, pageParam, signal),
    getNextPageParam: (page) => page.events.length === 100 ? page.nextAfterId : undefined,
    refetchInterval: 10_000,
  });
  const events = useMemo(() => activity.data?.pages.flatMap((page) => page.events) || [], [activity.data]);

  if (run.data?.run && run.data.run.projectId !== projectId) {
    return <Navigate to="/projects/$projectId/runs/$runId" params={{ projectId: run.data.run.projectId, runId }} replace />;
  }

  return <PageShell>
    <PageHeader eyebrow="Project / Run" title={run.data?.run?.launchKey || "Run"} description="Execution record and durable event history."
      actions={<button type="button" className="secondary-button" onClick={() => { void run.refetch(); void activity.refetch(); }}><RefreshCw size={15} aria-hidden="true" /> Refresh</button>} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Project runs</Link>
    {run.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading run</h2></div> : null}
    {run.isError ? <div className="state-panel" role="alert"><h2>Run unavailable</h2><p>This run could not be loaded.</p><button type="button" className="secondary-button" onClick={() => void run.refetch()}>Try again</button></div> : null}
    {run.data?.run ? <div className="summary-grid">
      <section className="summary-card"><span className="summary-label">State</span><strong className="summary-value"><span className={`state-badge state-${run.data.run.state}`}>{run.data.run.state.replaceAll("_", " ")}</span></strong><span className="summary-meta">Engineering execution</span></section>
      <section className="summary-card"><span className="summary-label">Source commit</span><strong className="summary-value mono" title={run.data.run.sourceCommit}>{run.data.run.sourceCommit.slice(0, 12)}</strong><span className="summary-meta">Pinned at launch</span></section>
      <section className="summary-card"><span className="summary-label">Created</span><strong className="summary-value"><time dateTime={run.data.run.createdAt}>{new Date(run.data.run.createdAt).toLocaleDateString()}</time></strong><span className="summary-meta">{new Date(run.data.run.createdAt).toLocaleTimeString()}</span></section>
    </div> : null}
    <section className="table-section" aria-labelledby="activity-heading">
      <div className="table-heading"><div><h2 id="activity-heading">Activity</h2><p>Committed events are shown in sequence and can be refreshed after reconnecting.</p></div><span className="fetched-time">{events.length} events</span></div>
      {run.data?.run && activity.isPending ? <div className="table-empty" role="status">Loading activity…</div> : null}
      {activity.isError ? <div className="table-empty" role="alert">Activity is unavailable. <button type="button" className="text-action" onClick={() => void activity.refetch()}>Try again</button></div> : null}
      {activity.data && events.length === 0 ? <div className="table-empty">No activity has been recorded yet.</div> : null}
      {events.length > 0 ? <ol className="event-list">{events.map((event) => <li key={event.id.toString()}><span className="event-mark"><GitCommitHorizontal size={15} aria-hidden="true" /></span><div><strong>{event.kind.replaceAll(".", " · ").replaceAll("_", " ")}</strong><small>{event.taskId ? `Task ${event.taskId.slice(0, 8)} · ` : ""}{event.attemptId ? `Attempt ${event.attemptId.slice(0, 8)}` : ""}</small></div><time dateTime={event.occurredAt}>{new Date(event.occurredAt).toLocaleString()}</time></li>)}</ol> : null}
      {activity.hasNextPage ? <div className="table-footer"><span>Older activity is available</span><button type="button" className="secondary-button" disabled={activity.isFetchingNextPage} onClick={() => void activity.fetchNextPage()}>{activity.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
    </section>
  </PageShell>;
}
