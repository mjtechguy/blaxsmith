import { useEffect, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { rowSortingFeature, tableFeatures, useTable, type ColumnDef, type SortingState } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, GitBranch, RefreshCw, Search } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { Run } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getProject, listRuns, runQueries } from "../workflow";

export const Route = createFileRoute("/projects/$projectId")({ component: ProjectRuns });

const features = tableFeatures({ rowSortingFeature });
const columns: ColumnDef<typeof features, Run>[] = [
  { id: "run", accessorKey: "launchKey", header: "Run", cell: ({ row }) =>
    <Link className="run-link" to="/projects/$projectId/runs/$runId" params={{ projectId: row.original.projectId, runId: row.original.id }}>
      <span className="project-symbol"><GitBranch size={15} aria-hidden="true" /></span>
      <span><strong>{row.original.launchKey}</strong><small>{row.original.sourceCommit.slice(0, 12)}</small></span>
      <ArrowRight size={15} aria-hidden="true" />
    </Link> },
  { id: "state", accessorKey: "state", header: "State", cell: ({ row }) => <span className={`state-badge state-${row.original.state}`}>{row.original.state.replaceAll("_", " ")}</span> },
  { id: "created", accessorKey: "createdAt", header: "Created", cell: ({ row }) => <time dateTime={row.original.createdAt}>{new Date(row.original.createdAt).toLocaleString()}</time> },
];

function ProjectRuns() {
  const { projectId } = Route.useParams();
  const runDetail = useMatchRoute()({ to: "/projects/$projectId/runs/$runId" });
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const [search, setSearch] = useState("");
  const [submittedSearch, setSubmittedSearch] = useState("");
  const [sorting, setSorting] = useState<SortingState>([{ id: "created", desc: true }]);
  useEffect(() => { const timer = window.setTimeout(() => setSubmittedSearch(search.trim()), 250); return () => window.clearTimeout(timer); }, [search]);
  const sortBy = sorting[0]?.id === "run" ? "launch_key" : sorting[0]?.id === "state" ? "state" : "created_at";
  const sortDirection = sorting[0]?.desc ? "desc" : "asc";
  const runs = useInfiniteQuery({
    queryKey: runQueries(org, projectId, submittedSearch, sortBy, sortDirection), enabled: Boolean(org && project.data?.project), initialPageParam: "",
    queryFn: ({ pageParam, signal }) => listRuns(projectId, pageParam, submittedSearch, sortBy, sortDirection, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => runs.data?.pages.flatMap((page) => page.runs) || [], [runs.data]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.id,
    manualSorting: true, enableMultiSort: false, enableSortingRemoval: false, state: { sorting }, onSortingChange: setSorting });

  if (runDetail) return <Outlet />;

  return <PageShell>
    <PageHeader eyebrow="Workspace / Project" title={project.data?.project?.name || "Project runs"} description={project.data?.project ? `Project URL: ${project.data.project.slug}` : "Runs and evidence for this project."} />
    <Link to="/" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> All projects</Link>
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {project.data?.project && runs.isPending ? <div className="state-panel" role="status"><RefreshCw size={22} className="spin" aria-hidden="true" /><h2>Loading runs</h2></div> : null}
    {runs.isError ? <div className="state-panel" role="alert"><h2>Runs unavailable</h2><p>Could not load runs for this project.</p><button className="secondary-button" type="button" onClick={() => void runs.refetch()}>Try again</button></div> : null}
    {runs.data && rows.length === 0 && !submittedSearch ? <section className="empty-card"><div className="empty-icon"><GitBranch size={22} aria-hidden="true" /></div><h2>No runs yet</h2><p>Runs will appear here after an approved recipe and execution environment are connected.</p></section> : null}
    {rows.length > 0 || search || submittedSearch ? <section className="table-section" aria-labelledby="runs-heading"><div className="table-heading"><div><h2 id="runs-heading">Runs</h2><p>Open a run to inspect its recorded activity.</p></div><span className="fetched-time">{rows.length} loaded</span></div>
      <div className="table-toolbar"><label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search runs</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search run keys or commits" maxLength={120} /></label></div>
      <DataTable table={table} label="Project runs" empty={runs.isPending || runs.isError ? undefined : "No runs match this search."} />
      <div className="table-footer"><span>{rows.length} loaded in server order</span>{runs.hasNextPage ? <button type="button" className="secondary-button" disabled={runs.isFetchingNextPage} onClick={() => void runs.fetchNextPage()}>{runs.isFetchingNextPage ? "Loading…" : "Load more"}</button> : null}</div>
    </section> : null}
  </PageShell>;
}
