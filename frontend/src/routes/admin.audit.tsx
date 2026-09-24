import { useEffect, useMemo, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, RefreshCw, Search, ShieldAlert } from "lucide-react";
import { auditActions, auditKey, isOrgAdmin, listAuditEvents } from "../admin";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { AdminAuditEvent } from "../gen/blaxsmith/api/v1/admin_pb";
import { PageHeader, PageShell } from "../page";
import { listProjects } from "../workflow";

export const Route = createFileRoute("/admin/audit")({ component: AuditLog });

const features = tableFeatures({});
const columns: ColumnDef<typeof features, AdminAuditEvent>[] = [
  { id: "when", header: "When", cell: ({ row }) => <time dateTime={row.original.occurredAt}>{new Date(row.original.occurredAt).toLocaleString()}</time> },
  { id: "action", header: "Kind", cell: ({ row }) => <span className="mono">{row.original.action}</span> },
  { id: "actor", header: "Actor", cell: ({ row }) => row.original.actorUsername
    ? <span title={row.original.actorId}>{row.original.actorUsername}</span> : <span className="state-badge">{row.original.actorKind}</span> },
  { id: "project", header: "Project", cell: ({ row }) => row.original.projectName || "—" },
  { id: "subject", header: "Subject", cell: ({ row }) => row.original.subjectId ? <code title={row.original.subjectId}>{row.original.subjectId.slice(0, 8)}</code> : "—" },
];

function AuditLog() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const allowed = isOrgAdmin(session.data);
  const [action, setAction] = useState("");
  const [projectId, setProjectId] = useState("");
  const [actorInput, setActorInput] = useState("");
  const [actor, setActor] = useState("");
  useEffect(() => { const timer = window.setTimeout(() => setActor(actorInput.trim()), 300); return () => window.clearTimeout(timer); }, [actorInput]);
  // ponytail: the project filter offers the first 20 projects by recency.
  const projects = useQuery({ queryKey: ["admin-audit-projects", org], enabled: Boolean(org && allowed), queryFn: ({ signal }) => listProjects("", "", "created_at", "desc", signal) });
  const events = useInfiniteQuery({
    queryKey: auditKey(org, action, actor, projectId), enabled: Boolean(org && allowed), initialPageParam: "",
    queryFn: ({ pageParam, signal }) => listAuditEvents(pageParam, action, actor, projectId, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => events.data?.pages.flatMap((page) => page.events) || [], [events.data]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.id.toString() });

  if (session.data && !allowed) return <PageShell><div className="state-panel" role="alert"><ShieldAlert size={22} aria-hidden="true" /><h2>Administration is restricted</h2><p>Only organization owners and admins can read the audit log.</p><Link className="secondary-button" to="/">Back to workspace</Link></div></PageShell>;
  const denied = events.isError && ConnectError.from(events.error).code === Code.PermissionDenied;

  return <PageShell>
    <PageHeader eyebrow="Administration" title="Audit log" description="Append-only security and operations events for this organization, newest first." />
    <Link to="/admin" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Operations</Link>
    <section className="table-section" aria-labelledby="audit-heading">
      <div className="table-heading"><div><h2 id="audit-heading">Events</h2><p>Filter by kind, actor, or project. Secrets, tokens, and credential material are never recorded.</p></div>
        <button type="button" className="secondary-button" disabled={events.isFetching} onClick={() => void events.refetch()}><RefreshCw size={14} aria-hidden="true" className={events.isFetching ? "spin" : undefined} /> Refresh</button></div>
      <div className="table-toolbar">
        <label className="filter-field"><span className="sr-only">Filter by kind</span>
          <select value={action} onChange={(event) => setAction(event.target.value)}><option value="">All kinds</option>{auditActions.map((entry) => <option key={entry} value={entry}>{entry}</option>)}</select></label>
        <label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Filter by actor</span>
          <input value={actorInput} onChange={(event) => setActorInput(event.target.value)} placeholder="Actor username or id" maxLength={64} /></label>
        <label className="filter-field"><span className="sr-only">Filter by project</span>
          <select value={projectId} onChange={(event) => setProjectId(event.target.value)}><option value="">All projects</option>{projects.data?.projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
      </div>
      {events.isPending ? <div className="source-summary" role="status">Loading events…</div> : null}
      {events.isError ? <div className="source-summary" role="alert">{denied ? "Your session is not an organization owner or admin." : "Audit events could not be loaded."} {denied ? null : <button type="button" className="text-action" onClick={() => void events.refetch()}>Try again</button>}</div> : null}
      <DataTable table={table} label="Audit events" empty={events.isPending || events.isError ? undefined : "No events match these filters."} />
      <div className="table-footer"><span>{rows.length} loaded in server order</span>{events.hasNextPage ? <button type="button" className="secondary-button" disabled={events.isFetchingNextPage} onClick={() => void events.fetchNextPage()}>{events.isFetchingNextPage ? "Loading…" : "Load more"}</button> : null}</div>
    </section>
  </PageShell>;
}
