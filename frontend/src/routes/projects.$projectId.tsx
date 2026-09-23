import { useMemo } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, GitBranch, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { Run } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getProject, listRuns, runQueries } from "../workflow";

export const Route = createFileRoute("/projects/$projectId")({ component: ProjectRuns });

const features = tableFeatures({});
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
  const project = useQuery({ queryKey: ["project", projectId], queryFn: ({ signal }) => getProject(projectId, signal) });
  const runs = useInfiniteQuery({
    queryKey: runQueries(session.data?.organizationId || "", projectId), enabled: Boolean(session.data?.organizationId && project.data?.project), initialPageParam: "",
    queryFn: ({ pageParam, signal }) => listRuns(projectId, pageParam, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => runs.data?.pages.flatMap((page) => page.runs) || [], [runs.data]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.id });

  if (runDetail) return <Outlet />;

  return <PageShell>
    <PageHeader eyebrow="Workspace / Project" title={project.data?.project?.name || "Project runs"} description={project.data?.project ? `Project URL: ${project.data.project.slug}` : "Runs and evidence for this project."} />
    <Link to="/" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> All projects</Link>
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {project.data?.project && runs.isPending ? <div className="state-panel" role="status"><RefreshCw size={22} className="spin" aria-hidden="true" /><h2>Loading runs</h2></div> : null}
    {runs.isError ? <div className="state-panel" role="alert"><h2>Runs unavailable</h2><p>Could not load runs for this project.</p><button className="secondary-button" type="button" onClick={() => void runs.refetch()}>Try again</button></div> : null}
    {runs.data && rows.length === 0 ? <section className="empty-card"><div className="empty-icon"><GitBranch size={22} aria-hidden="true" /></div><h2>No runs yet</h2><p>Runs will appear here after an approved recipe and execution environment are connected.</p></section> : null}
    {rows.length > 0 ? <section className="table-section" aria-labelledby="runs-heading"><div className="table-heading"><div><h2 id="runs-heading">Recent runs</h2><p>Open a run to inspect its recorded activity.</p></div><span className="fetched-time">{rows.length} loaded</span></div>
      <DataTable table={table} label="Project runs" />
      {runs.hasNextPage ? <div className="table-footer"><span>Most recent first</span><button type="button" className="secondary-button" disabled={runs.isFetchingNextPage} onClick={() => void runs.fetchNextPage()}>{runs.isFetchingNextPage ? "Loading…" : "Load more"}</button></div> : null}
    </section> : null}
  </PageShell>;
}
