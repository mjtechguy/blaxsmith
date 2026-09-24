import { useEffect, useMemo, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { keepPreviousData, useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { RefreshCw, Search } from "lucide-react";
import { auditActions, auditKey, listAuditEvents } from "../admin";
import { CollectionTable, useLocalView, type GridColumn } from "../data-table";
import type { AdminAuditEvent } from "../gen/blaxsmith/api/v1/admin_pb";
import { PageHeader, PageShell } from "../page";
import { CopyValue, EmptyState, Timestamp } from "../ui";
import { useScope } from "../workspace-ui";
import { listProjects } from "../workflow";

type AuditSearch = { action?: string; actor?: string; project?: string };
export const Route = createFileRoute("/admin/audit")({
  validateSearch: (search: Record<string, unknown>): AuditSearch => ({
    ...(typeof search.action === "string" && auditActions.includes(search.action) ? { action: search.action } : {}),
    ...(typeof search.actor === "string" && search.actor ? { actor: search.actor.slice(0, 64) } : {}),
    ...(typeof search.project === "string" && search.project ? { project: search.project } : {}),
  }),
  component: AuditLog,
});

const columns: GridColumn<AdminAuditEvent>[] = [
  { id: "when", accessorKey: "occurredAt", header: "When", enableHiding: false, cell: ({ row }) => <Timestamp value={row.original.occurredAt} /> },
  { id: "action", accessorKey: "action", header: "Event", cell: ({ row }) => <span className="mono">{row.original.action}</span> },
  { id: "actor", accessorKey: "actorUsername", header: "Actor", cell: ({ row }) => row.original.actorUsername || <span className="state-badge">{row.original.actorKind}</span> },
  { id: "project", accessorKey: "projectName", header: "Project", cell: ({ row }) => row.original.projectName || "—" },
  { id: "subject", accessorKey: "subjectId", header: "Subject", cell: ({ row }) => <CopyValue value={row.original.subjectId} label="Subject ID" /> },
];

// Filters are server-side and live in the URL; the server pages newest first by cursor.
function AuditLog() {
  const { org } = useScope();
  const search = Route.useSearch();
  const navigate = useNavigate();
  const action = search.action ?? "";
  const actor = search.actor ?? "";
  const projectId = search.project ?? "";
  const [actorInput, setActorInput] = useState(actor);
  const setFilter = (patch: AuditSearch) => void navigate({ to: "/admin/audit", search: (prev: AuditSearch) => {
    const next = { ...prev, ...patch };
    return Object.fromEntries(Object.entries(next).filter(([, v]) => v)) as AuditSearch;
  }, replace: true });
  useEffect(() => { const timer = window.setTimeout(() => { if (actorInput.trim() !== actor) setFilter({ actor: actorInput.trim() || undefined }); }, 300); return () => window.clearTimeout(timer); });
  // ponytail: the project filter offers the first 20 projects by recency.
  const projects = useQuery({ queryKey: ["admin-audit-projects", org], enabled: Boolean(org), queryFn: ({ signal }) => listProjects("", "", "created_at", "desc", signal) });
  const events = useInfiniteQuery({
    queryKey: auditKey(org, action, actor, projectId), enabled: Boolean(org), initialPageParam: "", placeholderData: keepPreviousData,
    queryFn: ({ pageParam, signal }) => listAuditEvents(pageParam, action, actor, projectId, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => events.data?.pages.flatMap((page) => page.events) || [], [events.data]);
  const [view, setView] = useLocalView({ size: 50 });
  const denied = events.isError && ConnectError.from(events.error).code === Code.PermissionDenied;

  return <PageShell>
    <PageHeader title="Audit log" description="Append-only security and operations events for this organization, newest first. Secrets, tokens, and credential material are never recorded."
      actions={<button type="button" className="secondary-button" disabled={events.isFetching} onClick={() => void events.refetch()}><RefreshCw size={14} aria-hidden="true" className={events.isFetching ? "spin" : undefined} /> Refresh</button>} />
    <section className="table-section" aria-label="Audit events">
      <div className="grid-toolbar">
        <label className="filter-field"><span className="filter-label">Event</span>
          <select value={action} onChange={(event) => setFilter({ action: event.target.value || undefined })}><option value="">All events</option>{auditActions.map((entry) => <option key={entry} value={entry}>{entry}</option>)}</select></label>
        <label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Actor</span>
          <input value={actorInput} onChange={(event) => setActorInput(event.target.value)} placeholder="Actor username or ID" maxLength={64} /></label>
        <label className="filter-field"><span className="filter-label">Project</span>
          <select value={projectId} onChange={(event) => setFilter({ project: event.target.value || undefined })}><option value="">All projects</option>{projects.data?.projects.map((project) => <option key={project.id} value={project.id}>{project.name}</option>)}</select></label>
      </div>
      <CollectionTable id="admin-audit" label="Audit events" noun="events" columns={columns} data={rows} getRowId={(e) => e.id.toString()} view={view} onView={setView} paged={false} sortable={false}
        loading={events.isPending} refreshing={events.isFetching && !events.isPending}
        error={events.isError ? denied ? "Your session is not an organization owner or admin." : <>Audit events could not be loaded. <button type="button" className="text-action" onClick={() => void events.refetch()}>Try again</button></> : undefined}
        empty={<EmptyState title={action || actor || projectId ? "No events match these filters" : "No events recorded"}>{action || actor || projectId ? "Clear a filter to see more." : null}</EmptyState>} />
      <div className="table-footer"><span>{rows.length} loaded, newest first</span>{events.hasNextPage ? <button type="button" className="secondary-button" disabled={events.isFetchingNextPage} onClick={() => void events.fetchNextPage()}>{events.isFetchingNextPage ? "Loading…" : "Load more"}</button> : null}</div>
    </section>
  </PageShell>;
}
