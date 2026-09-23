import { useMemo } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowRight, FolderKanban, Plus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import type { Project } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { listProjects, projectQueries } from "../workflow";

export const Route = createFileRoute("/")({ component: Workspace });

const features = tableFeatures({});
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
  const projects = useInfiniteQuery({
    queryKey: projectQueries(org), enabled: Boolean(org), initialPageParam: "",
    queryFn: ({ pageParam, signal }) => listProjects(pageParam, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => projects.data?.pages.flatMap((page) => page.projects) || [], [projects.data]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.id });

  return <PageShell>
    <PageHeader eyebrow="Workspace" title="Engineering work" description="Projects and their agent runs, with evidence and decisions attached to each run."
      actions={canCreate ? <Link className="primary-button" to="/projects/new"><Plus size={16} aria-hidden="true" /> Add project</Link> : undefined} />
    {projects.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading projects</h2><p>Connecting to your organization’s workspace.</p></div> : null}
    {projects.isError ? <div className="state-panel" role="alert"><h2>Projects unavailable</h2><p>Could not load your projects.</p><button className="secondary-button" type="button" onClick={() => void projects.refetch()}>Try again</button></div> : null}
    {projects.data && rows.length === 0 ? <section className="empty-card" aria-labelledby="workspace-empty-title">
      <div className="empty-icon"><FolderKanban size={22} aria-hidden="true" /></div>
      <h2 id="workspace-empty-title">No projects yet</h2>
      <p>Projects keep an engineering goal, its runs, and the final review together.</p>
      {canCreate ? <Link to="/projects/new" className="primary-button"><Plus size={16} aria-hidden="true" /> Add project</Link> : <p>Ask an organization member to create the first project.</p>}
    </section> : null}
    {rows.length > 0 ? <section className="table-section" aria-labelledby="projects-heading">
      <div className="table-heading"><div><h2 id="projects-heading">Projects</h2><p>Open a project to see its runs and evidence.</p></div><span className="fetched-time">{rows.length} loaded</span></div>
      <div className="table-scroll"><table><thead>{table.getHeaderGroups().map((group) => <tr key={group.id}>{group.headers.map((header) => <th key={header.id} scope="col"><span className="table-label"><table.FlexRender header={header} /></span></th>)}</tr>)}</thead>
        <tbody>{table.getRowModel().rows.map((row) => <tr key={row.id}>{row.getAllCells().map((cell) => <td key={cell.id}><table.FlexRender cell={cell} /></td>)}</tr>)}</tbody></table></div>
      {projects.hasNextPage ? <div className="table-footer"><span>Most recent first</span><button className="secondary-button" type="button" disabled={projects.isFetchingNextPage} onClick={() => void projects.fetchNextPage()}>{projects.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
    </section> : null}
  </PageShell>;
}
