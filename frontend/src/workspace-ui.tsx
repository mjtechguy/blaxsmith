// Shared cells and rows for the cross-project work views (Home, Inbox, Runs).
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRight, CheckCircle2, CircleHelp, GitBranch, MessageSquare, ShieldAlert, TriangleAlert } from "lucide-react";
import { statusOrder, type AgentStatus } from "./agent-view";
import { currentSession, sessionQueryKey } from "./auth";
import { inSet, type GridColumn } from "./data-table";
import type { InboxItem, WorkspaceRun } from "./gen/blaxsmith/api/v1/workspace_pb";
import { StatusPill } from "./work-log";
import { CopyValue, Timestamp } from "./ui";
import { kindLabel } from "./workspace";
import { listRunTasks } from "./workflow";

export function useScope() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const role = session.data?.role ?? "";
  return {
    session, role, org: session.data?.organizationId ?? "",
    scope: session.data ? `${session.data.organizationId}:${session.data.principalId}` : "",
    isAdmin: role === "owner" || role === "admin",
    isMember: role === "owner" || role === "admin" || role === "member",
  };
}

export const asStatus = (status: string): AgentStatus | null => (statusOrder as readonly string[]).includes(status) ? status as AgentStatus : null;

export function RunStateBadge({ state }: { state: string }) {
  return <span className={`state-badge state-${state}`}>{state.replaceAll("_", " ")}</span>;
}

// Where an inbox item is handled: the run's review tab, its stage, or its inbox on the overview.
export function InboxLink({ item, children, className = "text-action" }: { item: InboxItem; children: React.ReactNode; className?: string }) {
  const params = { projectId: item.projectId, runId: item.runId };
  if (item.kind === "review") return <Link className={className} to="/projects/$projectId/runs/$runId" params={params} search={{ tab: "review" } as never}>{children}</Link>;
  if (item.kind === "interview_round") return <Link className={className} to="/projects/$projectId/runs/$runId" params={params} search={{ tab: "stages", stage: item.stage } as never}>{children}</Link>;
  return <Link className={className} to="/projects/$projectId/runs/$runId" params={params} hash="inbox-heading">{children}</Link>;
}

const kindIcon = { approval: ShieldAlert, escalation: TriangleAlert, review: CheckCircle2, interview_round: MessageSquare, question: CircleHelp } as const;

export function KindMark({ kind }: { kind: string }) {
  const Icon = kindIcon[kind as keyof typeof kindIcon] ?? CircleHelp;
  return <span className={`kind-mark kind-${kind}`} aria-hidden="true"><Icon size={15} /></span>;
}

export function inboxColumns(now?: number): GridColumn<InboxItem>[] {
  return [
    { id: "title", accessorKey: "title", header: "Item", enableHiding: false, cell: ({ row }) => <span className="inbox-cell"><KindMark kind={row.original.kind} />
      <span><InboxLink item={row.original} className="row-title">{row.original.title || kindLabel(row.original.kind)}</InboxLink>
        <small>{kindLabel(row.original.kind)}{row.original.blocking ? " · blocking" : ""}{row.original.canAct ? "" : " · view only"}</small></span></span> },
    { id: "kind", accessorKey: "kind", header: "Kind", filterFn: inSet, cell: ({ row }) => <span className={`state-badge kind-badge-${row.original.kind}`}>{kindLabel(row.original.kind)}</span> },
    { id: "project", accessorKey: "projectName", header: "Project", cell: ({ row }) => <Link className="text-link" to="/projects/$projectId" params={{ projectId: row.original.projectId }}>{row.original.projectName}</Link> },
    { id: "run", accessorKey: "runLaunchKey", header: "Run · stage", cell: ({ row }) => <span className="task-stage"><strong>{row.original.runLaunchKey}</strong><small>{row.original.stage || "Final review"}</small></span> },
    { id: "age", accessorKey: "createdAt", header: "Waiting", enableSorting: false, cell: ({ row }) => <Timestamp value={row.original.createdAt} now={now} /> },
    { id: "act", header: "Action", enableSorting: false, enableHiding: false, cell: ({ row }) => <InboxLink item={row.original}>{row.original.canAct ? "Open" : "View"} <ArrowRight size={13} aria-hidden="true" /></InboxLink> },
  ];
}

export function useRunColumns(opts: { showProject: boolean }): GridColumn<WorkspaceRun>[] {
  return useMemo(() => {
    const cols: GridColumn<WorkspaceRun>[] = [
      { id: "run", accessorKey: "launchKey", header: "Run", enableHiding: false, cell: ({ row }) => <Link className="run-link" to="/projects/$projectId/runs/$runId" params={{ projectId: row.original.projectId, runId: row.original.id }}>
        <span className="project-symbol"><GitBranch size={15} aria-hidden="true" /></span><span><strong>{row.original.launchKey}</strong><small>{row.original.sourceCommit.slice(0, 12)}</small></span></Link> },
    ];
    if (opts.showProject) cols.push({ id: "project", accessorKey: "projectName", header: "Project", cell: ({ row }) => <Link className="text-link" to="/projects/$projectId" params={{ projectId: row.original.projectId }}>{row.original.projectName}</Link> });
    cols.push(
      { id: "status", accessorKey: "status", header: "Status", enableSorting: false, filterFn: inSet, cell: ({ row }) => <StatusPill status={asStatus(row.original.status)} title={`State: ${row.original.state.replaceAll("_", " ")}`} /> },
      { id: "state", accessorKey: "state", header: "State", filterFn: inSet, cell: ({ row }) => <RunStateBadge state={row.original.state} /> },
      { id: "stages", header: "Stages", enableSorting: false, cell: ({ row }) => row.original.stageCount ? <span className="progress-cell"><meter min={0} max={row.original.stageCount} value={row.original.stagesSucceeded} aria-label={`${row.original.stagesSucceeded} of ${row.original.stageCount} stages succeeded`} />
        <span>{row.original.stagesSucceeded}/{row.original.stageCount}</span></span> : <span className="muted">—</span> },
      { id: "waiting", header: "Waiting", enableSorting: false, cell: ({ row }) => row.original.openInteractions || row.original.reviewWaiting
        ? <span className="state-badge state-waiting">{[row.original.openInteractions ? `${row.original.openInteractions} open` : "", row.original.reviewWaiting ? "review" : ""].filter(Boolean).join(" · ")}</span> : <span className="muted">—</span> },
      { id: "commit", accessorKey: "sourceCommit", header: "Commit", enableSorting: false, cell: ({ row }) => <CopyValue value={row.original.sourceCommit} label="Source commit" chars={10} /> },
      { id: "created", accessorKey: "createdAt", header: "Created", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
    );
    return cols;
  }, [opts.showProject]);
}

// Expanded run row: its stages with status pills, loaded on first expand.
export function RunStagesDetail({ run, scope }: { run: WorkspaceRun; scope: string }) {
  const tasks = useQuery({ queryKey: ["run-tasks", scope, run.id], queryFn: ({ signal }) => listRunTasks(run.id, signal), staleTime: 10_000 });
  if (tasks.isPending) return <p className="row-detail muted" role="status">Loading stages…</p>;
  if (tasks.isError) return <p className="row-detail" role="alert">Stages could not be loaded. <button type="button" className="text-action" onClick={() => void tasks.refetch()}>Try again</button></p>;
  if (!tasks.data.tasks.length) return <p className="row-detail muted">No stages have been frozen for this run yet.</p>;
  return <div className="row-detail">
    <ol className="stage-strip" aria-label={`Stages of ${run.launchKey}`}>{tasks.data.tasks.map((task) => <li key={task.id}>
      <Link to="/projects/$projectId/runs/$runId" params={{ projectId: run.projectId, runId: run.id }} search={{ tab: "stages", stage: task.key } as never} className="stage-chip">
        <strong>{task.key}</strong><span className={`state-badge state-${task.state}`}>{task.state.replaceAll("_", " ")}</span>
        <small>{task.kind === "human_review" ? "Human review" : [task.harness, task.model].filter(Boolean).join(" · ")}</small></Link></li>)}</ol>
    <Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: run.projectId, runId: run.id }} search={{ tab: "stages" } as never}>Open stages <ArrowRight size={13} aria-hidden="true" /></Link>
  </div>;
}
