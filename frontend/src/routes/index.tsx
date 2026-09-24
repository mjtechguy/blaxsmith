import { useEffect, useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { Activity, ArrowRight, Cable, FolderPlus, Inbox, ShieldCheck } from "lucide-react";
import { DataTable } from "../data-table";
import type { WorkspaceAgent } from "../gen/blaxsmith/api/v1/workspace_pb";
import { DashboardLayout } from "../layouts";
import { Slot } from "../slots";
import { Card, EmptyState, StatePanel, StatTile, Timestamp } from "../ui";
import { StatusPill, useAttentionTitle } from "../work-log";
import { asStatus, InboxLink, KindMark, RunStateBadge, useScope } from "../workspace-ui";
import { getWorkspaceHome, homeKey, HOME_REFRESH_MS, kindLabel } from "../workspace";

export const Route = createFileRoute("/")({ component: Home });

const agentFeatures = tableFeatures({});

function Home() {
  const { scope, role, isAdmin, isMember } = useScope();
  const home = useQuery({ queryKey: homeKey(scope), enabled: Boolean(scope), queryFn: ({ signal }) => getWorkspaceHome(signal), refetchInterval: HOME_REFRESH_MS });
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => { setNow(Date.now()); }, [home.dataUpdatedAt]);
  const data = home.data;
  useAttentionTitle(data ? data.waitingOnYou : null);
  const agentColumns = useMemo<ColumnDef<typeof agentFeatures, WorkspaceAgent>[]>(() => [
    { id: "run", header: "Project · run", cell: ({ row }) => <span className="task-stage"><strong>{row.original.projectName}</strong>
      <Link className="text-link" to="/projects/$projectId/runs/$runId" params={{ projectId: row.original.projectId, runId: row.original.runId }} search={{ tab: "stages", stage: row.original.stage } as never}>{row.original.runLaunchKey}</Link></span> },
    { id: "stage", header: "Stage", cell: ({ row }) => <span className="task-stage"><strong>{row.original.stage}</strong><small>{row.original.kind.replaceAll("_", " ") || "—"}</small></span> },
    { id: "runtime", header: "Harness · model", cell: ({ row }) => <span className="task-stage"><strong>{row.original.harness || "—"}</strong><small className="mono">{row.original.model || "—"}</small></span> },
    { id: "control", header: "Control", cell: ({ row }) => row.original.takenOver ? <span className="control-pill control-other">Human</span> : <span className="control-pill control-agent">Agent</span> },
    { id: "activity", header: "Last activity", cell: ({ row }) => <Timestamp value={row.original.lastActivityAt || row.original.startedAt} now={now} /> },
  ], [now]);
  const agents = useTable({ features: agentFeatures, data: data?.agents ?? [], columns: agentColumns, getRowId: (row) => row.attemptId });

  return <DashboardLayout title="Home" description="What needs you, what is running, and what finished recently across your organization's projects."
    actions={isMember ? <Link className="primary-button" to="/projects/new"><FolderPlus size={15} aria-hidden="true" /> New project</Link> : undefined}
    tiles={data ? <>
      <StatTile label="Waiting on you" value={data.waitingOnYou} tone={data.waitingOnYou ? "attention" : undefined} href="/inbox" meta={`${data.openItems} open in total`} />
      <StatTile label="Running agents" value={data.runningAgents} meta="Attempts in flight" href="/runs?f_state=active" />
      <StatTile label="Active runs" value={data.activeRuns} meta="Queued or running" href="/runs?f_state=queued,active,cancel_requested" />
      <StatTile label="Runs in 24 hours" value={data.runsLast24h} meta="Started" />
      <StatTile label="Failed in 24 hours" value={data.failedLast24h} tone={data.failedLast24h ? "danger" : undefined} href="/runs?f_state=failed" />
    </> : undefined}
    slot={role ? <Slot name="home.checklist" role={role} /> : null}>
    {home.isPending ? <StatePanel kind="loading" title="Loading your workspace" /> : null}
    {home.isError ? <StatePanel kind="error" title="Home is unavailable" retry={() => void home.refetch()}>The workspace summary could not be loaded.</StatePanel> : null}
    {data ? <>
      <section className="ledger dash-wide" aria-labelledby="waiting-heading">
        <header className="ledger-header">
          <div><h2 id="waiting-heading"><Inbox size={16} aria-hidden="true" /> Waiting on you</h2>
            <p>{role === "viewer" ? "Viewers follow work but do not answer agents or decide reviews." : "Open questions, approvals, and reviews you can act on. Blocking items come first."}</p></div>
          <Link className="secondary-button" to="/inbox">Open inbox <ArrowRight size={14} aria-hidden="true" /></Link>
        </header>
        {data.waiting.length ? <ol className="ledger-list">{data.waiting.map((item) => <li key={`${item.kind}-${item.id}`} className={`ledger-item kind-${item.kind}`}>
          <KindMark kind={item.kind} />
          <div className="ledger-main"><InboxLink item={item} className="row-title">{item.title || kindLabel(item.kind)}</InboxLink>
            <small>{kindLabel(item.kind)}{item.blocking ? " · blocking" : ""} · {item.projectName} · {item.runLaunchKey}{item.stage ? ` · ${item.stage}` : ""}</small></div>
          <Timestamp value={item.createdAt} now={now} />
          <InboxLink item={item} className="secondary-button">Open</InboxLink>
        </li>)}</ol>
          : <EmptyState title="Nothing is waiting on you" action={<Link className="text-action" to="/runs">See all runs <ArrowRight size={13} aria-hidden="true" /></Link>}>
            {data.openItems ? `${data.openItems} open ${data.openItems === 1 ? "item needs" : "items need"} someone with a different role.` : "Agents will ask here when they need a decision."}</EmptyState>}
        {data.waitingOnYou > data.waiting.length ? <p className="ledger-more"><Link className="text-action" to="/inbox">Show {data.waitingOnYou - data.waiting.length} more in the inbox</Link></p> : null}
      </section>

      <Card title={<><Activity size={15} aria-hidden="true" /> Running agents</>} description="Current attempt owners, oldest first." className="dash-main"
        actions={isAdmin ? <Link className="text-action" to="/admin">Operations <ArrowRight size={13} aria-hidden="true" /></Link> : undefined}>
        {data.agents.length ? <DataTable table={agents} label="Running agents" />
          : <EmptyState title="No agents are running">Start a run from a project to see its agents here.</EmptyState>}
      </Card>

      <Card title="Quick actions" className="dash-side">
        <ul className="quick-actions">
          {isMember ? <li><Link to="/projects/new"><FolderPlus size={16} aria-hidden="true" /><span><strong>New project</strong><small>Connect a repository and checks</small></span></Link></li> : null}
          <li><Link to="/inbox"><Inbox size={16} aria-hidden="true" /><span><strong>Inbox</strong><small>Everything open across projects</small></span></Link></li>
          <li><Link to="/runs"><Activity size={16} aria-hidden="true" /><span><strong>All runs</strong><small>Filter by state and project</small></span></Link></li>
          <li><Link to="/me/connections"><Cable size={16} aria-hidden="true" /><span><strong>My connections</strong><small>Your own keys and subscriptions</small></span></Link></li>
          {isAdmin ? <li><Link to="/admin"><ShieldCheck size={16} aria-hidden="true" /><span><strong>Operations</strong><small>Capacity, grants, and live agents</small></span></Link></li> : null}
        </ul>
      </Card>

      <Card title="Recent runs" description="The newest runs in your organization." className="dash-wide"
        actions={<Link className="text-action" to="/runs">All runs <ArrowRight size={13} aria-hidden="true" /></Link>}>
        {data.recentRuns.length ? <ul className="run-list">{data.recentRuns.map((run) => <li key={run.id}>
          <Link className="run-list-link" to="/projects/$projectId/runs/$runId" params={{ projectId: run.projectId, runId: run.id }}>
            <span className="run-list-main"><strong>{run.launchKey}</strong><small>{run.projectName}</small></span>
            <StatusPill status={asStatus(run.status)} /><RunStateBadge state={run.state} />
            <time dateTime={run.createdAt}>{new Date(run.createdAt).toLocaleString()}</time>
          </Link></li>)}</ul>
          : <EmptyState title="No runs yet" action={isMember ? <Link className="primary-button" to="/projects">Choose a project</Link> : undefined}>Runs appear here once a project launches one.</EmptyState>}
      </Card>
    </> : null}
  </DashboardLayout>;
}
