import { useEffect, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { rowSortingFeature, tableFeatures, useTable, type ColumnDef, type SortingState } from "@tanstack/react-table";
import { ArrowRight, FolderKanban, Plus, RefreshCw, Search } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { Project } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { listProjects, projectQueries } from "../workflow";

export const Route = createFileRoute("/")({ component: Workspace });

const features = tableFeatures({ rowSortingFeature });
const columns: ColumnDef<typeof features, Project>[] = [
  { id: "name", accessorKey: "name", header: "Project", cell: ({ row }) =>
    <Link className="project-link" to="/projects/$projectId" params={{ projectId: row.original.id }}>
      <span className="project-symbol"><FolderKanban size={16} aria-hidden="true" /></span>
      <span><strong>{row.original.name}</strong><small>{row.original.slug}</small></span>
      <ArrowRight size={15} aria-hidden="true" />
    </Link> },
  { id: "created", accessorKey: "createdAt", header: "Created", cell: ({ row }) =>
    <time dateTime={row.original.createdAt}>{new Date(row.original.createdAt).toLocaleDateString()}</time> },
];

function Workspace() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const canCreate = session.data?.role === "owner" || session.data?.role === "admin" || session.data?.role === "member";
  const [search, setSearch] = useState("");
  const [submittedSearch, setSubmittedSearch] = useState("");
  const [sorting, setSorting] = useState<SortingState>([{ id: "created", desc: true }]);
  useEffect(() => { const timer = window.setTimeout(() => setSubmittedSearch(search), 250); return () => window.clearTimeout(timer); }, [search]);
  const sortBy = sorting[0]?.id === "name" ? "name" : "created_at";
  const sortDirection = sorting[0]?.desc ? "desc" : "asc";
  const projects = useInfiniteQuery({
    queryKey: projectQueries(org, submittedSearch, sortBy, sortDirection), enabled: Boolean(org), initialPageParam: "",
    queryFn: ({ pageParam, signal }) => listProjects(pageParam, submittedSearch, sortBy, sortDirection, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => projects.data?.pages.flatMap((page) => page.projects) || [], [projects.data]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.id,
    manualSorting: true, enableMultiSort: false, enableSortingRemoval: false, state: { sorting }, onSortingChange: setSorting });

  return <PageShell>
    <PageHeader eyebrow="Workspace" title="Engineering work" description="Projects and their agent runs, with evidence and decisions attached to each run."
      actions={canCreate ? <Link className="primary-button" to="/projects/new"><Plus size={16} aria-hidden="true" /> Add project</Link> : undefined} />
    {projects.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading projects</h2><p>Connecting to your organization’s workspace.</p></div> : null}
    {projects.isError ? <div className="state-panel" role="alert"><h2>Projects unavailable</h2><p>Could not load your projects.</p><button className="secondary-button" type="button" onClick={() => void projects.refetch()}>Try again</button></div> : null}
    {projects.data && rows.length === 0 && !submittedSearch ? <section className="empty-card" aria-labelledby="workspace-empty-title">
      <div className="empty-icon"><FolderKanban size={22} aria-hidden="true" /></div>
      <h2 id="workspace-empty-title">No projects yet</h2>
      <p>Projects keep an engineering goal, its runs, and the final review together.</p>
      {canCreate ? <Link to="/projects/new" className="primary-button"><Plus size={16} aria-hidden="true" /> Add project</Link> : <p>Ask an organization member to create the first project.</p>}
    </section> : null}
    {rows.length > 0 || search || submittedSearch ? <section className="table-section" aria-labelledby="projects-heading">
      <div className="table-heading"><div><h2 id="projects-heading">Projects</h2><p>Open a project to see its runs and evidence.</p></div><span className="fetched-time">{rows.length} loaded</span></div>
      <div className="table-toolbar"><label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search projects</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search projects" maxLength={120} /></label></div>
      <DataTable table={table} label="Projects" empty={projects.isPending || projects.isError ? undefined : "No projects match this search."} />
      <div className="table-footer"><span>{rows.length} loaded in server order</span>{projects.hasNextPage ? <button className="secondary-button" type="button" disabled={projects.isFetchingNextPage} onClick={() => void projects.fetchNextPage()}>{projects.isFetchingNextPage ? "Loading…" : "Load more"}</button> : null}</div>
    </section> : null}
  </PageShell>;
}
