import { useEffect, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { rowSortingFeature, tableFeatures, useTable, type ColumnDef, type SortingState } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, GitBranch, Plus, RefreshCw, Search } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { Run } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getLaunchAvailability, getProject, getProjectSource, getProjectVerification, launchAvailabilityQueryKey, listRuns, projectSourceQueryKey, projectVerificationQueryKey, runQueries } from "../workflow";

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
  const sourceSettings = useMatchRoute()({ to: "/projects/$projectId/source" });
  const verificationSettings = useMatchRoute()({ to: "/projects/$projectId/verification" });
  const newRun = useMatchRoute()({ to: "/projects/$projectId/runs/new" });
  const childPage = Boolean(runDetail || sourceSettings || verificationSettings || newRun);
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org && !childPage), queryFn: ({ signal }) => getProject(projectId, signal) });
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: Boolean(org && !childPage && project.data?.project), queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: Boolean(org && !childPage && project.data?.project), queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const launchAvailability = useQuery({ queryKey: launchAvailabilityQueryKey(org, projectId), enabled: Boolean(org && !childPage && project.data?.project), queryFn: ({ signal }) => getLaunchAvailability(projectId, signal) });
  const mayEditSource = session.data?.role === "owner" || session.data?.role === "admin";
  const mayLaunch = mayEditSource || session.data?.role === "member";
  const [search, setSearch] = useState("");
  const [submittedSearch, setSubmittedSearch] = useState("");
  const [sorting, setSorting] = useState<SortingState>([{ id: "created", desc: true }]);
  useEffect(() => { const timer = window.setTimeout(() => setSubmittedSearch(search.trim()), 250); return () => window.clearTimeout(timer); }, [search]);
  const sortBy = sorting[0]?.id === "run" ? "launch_key" : sorting[0]?.id === "state" ? "state" : "created_at";
  const sortDirection = sorting[0]?.desc ? "desc" : "asc";
  const runs = useInfiniteQuery({
    queryKey: runQueries(org, projectId, submittedSearch, sortBy, sortDirection), enabled: Boolean(org && !childPage && project.data?.project), initialPageParam: "",
    queryFn: ({ pageParam, signal }) => listRuns(projectId, pageParam, submittedSearch, sortBy, sortDirection, signal),
    getNextPageParam: (page) => page.nextPageToken || undefined,
  });
  const rows = useMemo(() => runs.data?.pages.flatMap((page) => page.runs) || [], [runs.data]);
  const table = useTable({ features, data: rows, columns, getRowId: (row) => row.id,
    manualSorting: true, enableMultiSort: false, enableSortingRemoval: false, state: { sorting }, onSortingChange: setSorting });

  if (childPage) return <Outlet />;

  return <PageShell>
    <PageHeader eyebrow="Workspace / Project" title={project.data?.project?.name || "Project runs"} description={project.data?.project ? `Project URL: ${project.data.project.slug}` : "Runs and evidence for this project."}
      actions={mayLaunch && launchAvailability.data?.enabled && source.data && verification.data ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> New run</Link> : undefined} />
    <Link to="/" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> All projects</Link>
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {mayLaunch && launchAvailability.data && !launchAvailability.data.enabled ? <div className="notice" role="note"><strong>New runs are unavailable.</strong> {launchAvailability.data.reason}</div> : null}
    {mayLaunch && launchAvailability.isError ? <div className="notice" role="alert"><strong>Run availability could not be checked.</strong> <button type="button" className="text-action" onClick={() => void launchAvailability.refetch()}>Try again</button></div> : null}
    {project.data?.project ? <section className="table-section" aria-labelledby="source-heading">
      <div className="table-heading"><div><h2 id="source-heading">Git source</h2><p>The repository and ref used to prepare future runs.</p></div>
        {mayEditSource && source.isSuccess ? <Link className="secondary-button" to="/projects/$projectId/source" params={{ projectId }}>{source.data ? "Edit source" : <><Plus size={15} aria-hidden="true" /> Add source</>}</Link> : null}</div>
      {source.isPending ? <div className="source-summary" role="status">Loading source…</div> : null}
      {source.isError ? <div className="source-summary" role="alert">Source could not be loaded. <button type="button" className="text-action" onClick={() => void source.refetch()}>Try again</button></div> : null}
      {source.isSuccess ? <div className="source-summary">{source.data ? <><strong>{source.data.repositoryUrl}</strong><span>Ref: {source.data.ref || "Remote default branch"} · Updated {new Date(source.data.updatedAt).toLocaleString()}</span></> : <span>No Git source configured. Add a public GitHub or GitLab repository before starting a run.</span>}</div> : null}
    </section> : null}
    {project.data?.project ? <section className="table-section" aria-labelledby="verification-heading">
      <div className="table-heading"><div><h2 id="verification-heading">Verification</h2><p>Project checks required to validate run output.</p></div>
        {mayEditSource && verification.isSuccess ? <Link className="secondary-button" to="/projects/$projectId/verification" params={{ projectId }}>{verification.data ? "Edit checks" : <><Plus size={15} aria-hidden="true" /> Add checks</>}</Link> : null}</div>
      {verification.isPending ? <div className="source-summary" role="status">Loading checks…</div> : null}
      {verification.isError ? <div className="source-summary" role="alert">Verification could not be loaded. <button type="button" className="text-action" onClick={() => void verification.refetch()}>Try again</button></div> : null}
      {verification.isSuccess ? <div className="source-summary">{verification.data ? <><strong>{verification.data.checks.length} checks · Version {verification.data.version.toString()}</strong>{verification.data.checks.map((check) => <span key={check.id}><strong>{check.id}</strong> <code>{JSON.stringify(check.command)}</code></span>)}</> : <span>No verification checks configured. An owner or admin must add at least one before starting a run.</span>}</div> : null}
    </section> : null}
    {project.data?.project && runs.isPending ? <div className="state-panel" role="status"><RefreshCw size={22} className="spin" aria-hidden="true" /><h2>Loading runs</h2></div> : null}
    {runs.isError ? <div className="state-panel" role="alert"><h2>Runs unavailable</h2><p>Could not load runs for this project.</p><button className="secondary-button" type="button" onClick={() => void runs.refetch()}>Try again</button></div> : null}
    {runs.data && rows.length === 0 && !submittedSearch ? <section className="empty-card"><div className="empty-icon"><GitBranch size={22} aria-hidden="true" /></div><h2>No runs yet</h2><p>Configure a Git source and verification checks, then create the first run.</p></section> : null}
    {rows.length > 0 || search || submittedSearch ? <section className="table-section" aria-labelledby="runs-heading"><div className="table-heading"><div><h2 id="runs-heading">Runs</h2><p>Open a run to inspect its recorded activity.</p></div><span className="fetched-time">{rows.length} loaded</span></div>
      <div className="table-toolbar"><label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search runs</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search run keys or commits" maxLength={120} /></label></div>
      <DataTable table={table} label="Project runs" empty={runs.isPending || runs.isError ? undefined : "No runs match this search."} />
      <div className="table-footer"><span>{rows.length} loaded in server order</span>{runs.hasNextPage ? <button type="button" className="secondary-button" disabled={runs.isFetchingNextPage} onClick={() => void runs.fetchNextPage()}>{runs.isFetchingNextPage ? "Loading…" : "Load more"}</button> : null}</div>
    </section> : null}
  </PageShell>;
}
