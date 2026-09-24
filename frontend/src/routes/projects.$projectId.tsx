import { useEffect, useMemo, useState } from "react";
import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { rowSortingFeature, tableFeatures, useTable, type ColumnDef, type SortingState } from "@tanstack/react-table";
import { ArrowLeft, ArrowRight, GitBranch, Plus, RefreshCw, Search } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { Run } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getLaunchAvailability, getProject, getProjectSource, getProjectVerification, launchAvailabilityQueryKey, listProjectModelAccess, listRuns, projectModelAccessQueryKey, projectSourceQueryKey, projectVerificationQueryKey, runQueries } from "../workflow";

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
  const modelAccessSettings = useMatchRoute()({ to: "/projects/$projectId/model-access" });
  const newModelAccess = useMatchRoute()({ to: "/projects/$projectId/model-access/new" });
  const newRun = useMatchRoute()({ to: "/projects/$projectId/runs/new" });
  const childPage = Boolean(runDetail || sourceSettings || verificationSettings || modelAccessSettings || newModelAccess || newRun);
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org && !childPage), queryFn: ({ signal }) => getProject(projectId, signal) });
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: Boolean(org && !childPage && project.data?.project), queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: Boolean(org && !childPage && project.data?.project), queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const modelAccess = useQuery({ queryKey: projectModelAccessQueryKey(org, projectId), enabled: Boolean(org && !childPage && project.data?.project), queryFn: ({ signal }) => listProjectModelAccess(projectId, signal) });
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
    <PageHeader eyebrow="Workspace" title={project.data?.project?.name || "Workspace"} description={project.data?.project ? `Workspace URL: ${project.data.project.slug}` : "Configure a repository, model access, and run checks."} />
    <Link to="/" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> All workspaces</Link>
    {project.isError ? <div className="state-panel" role="alert"><h2>Workspace unavailable</h2><p>This workspace could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {mayLaunch && launchAvailability.data && !launchAvailability.data.enabled ? <div className="notice" role="note"><strong>New runs are unavailable.</strong> {launchAvailability.data.reason}</div> : null}
    {mayLaunch && launchAvailability.isError ? <div className="notice" role="alert"><strong>Run availability could not be checked.</strong> <button type="button" className="text-action" onClick={() => void launchAvailability.refetch()}>Try again</button></div> : null}
    {project.data?.project ? <section className="table-section" aria-labelledby="source-heading">
      <div className="table-heading"><div><h2 id="source-heading">1. Connect Git</h2><p>Choose the repository and ref used to prepare runs. Private repository credentials are not available yet.</p></div>
        {mayEditSource && source.isSuccess ? <Link className="secondary-button" to="/projects/$projectId/source" params={{ projectId }}>{source.data ? "Edit repository" : <><Plus size={15} aria-hidden="true" /> Add Git source</>}</Link> : null}</div>
      {source.isPending ? <div className="source-summary" role="status">Loading source…</div> : null}
      {source.isError ? <div className="source-summary" role="alert">Source could not be loaded. <button type="button" className="text-action" onClick={() => void source.refetch()}>Try again</button></div> : null}
      {source.isSuccess ? <div className="source-summary">{source.data ? <><strong>Source configured</strong><span>{source.data.repositoryUrl}</span><span>Ref: {source.data.ref || "Remote default branch"} · Updated {new Date(source.data.updatedAt).toLocaleString()}</span></> : <><strong>Setup needed</strong><span>An owner or admin must add a public GitHub or GitLab HTTPS repository before creating a run.</span></>}</div> : null}
    </section> : null}
    {project.data?.project ? <section className="table-section" aria-labelledby="model-access-heading">
      <div className="table-heading"><div><h2 id="model-access-heading">2. Grant model access</h2><p>Grant an organization provider key and exact model to this workspace when your recipe needs one.</p></div>
        {mayEditSource && modelAccess.isSuccess ? modelAccess.data.access.length === 0
          ? <Link className="secondary-button" to="/projects/$projectId/model-access/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add model access</Link>
          : <Link className="secondary-button" to="/projects/$projectId/model-access" params={{ projectId }}>Manage access <ArrowRight size={15} aria-hidden="true" /></Link>
          : modelAccess.isSuccess ? <Link className="secondary-button" to="/projects/$projectId/model-access" params={{ projectId }}>View access <ArrowRight size={15} aria-hidden="true" /></Link> : null}</div>
      {modelAccess.isPending ? <div className="source-summary" role="status">Loading model access…</div> : null}
      {modelAccess.isError ? <div className="source-summary" role="alert">Model access could not be loaded. <button type="button" className="text-action" onClick={() => void modelAccess.refetch()}>Try again</button></div> : null}
      {modelAccess.isSuccess ? <div className="source-summary">{modelAccess.data.access.length ? <><strong>{modelAccess.data.access.length} model {modelAccess.data.access.length === 1 ? "grant" : "grants"}</strong><span>{modelAccess.data.access.slice(0, 3).map((entry) => `${entry.provider} / ${entry.model}`).join(" · ")}{modelAccess.data.access.length > 3 ? ` · +${modelAccess.data.access.length - 3} more` : ""}</span></> : <><strong>No model access granted</strong><span>An owner or admin can add an organization API key. The key is write-only; the UI does not display it after saving.</span></>}</div> : null}
    </section> : null}
    {project.data?.project ? <section className="table-section" aria-labelledby="verification-heading">
      <div className="table-heading"><div><h2 id="verification-heading">3. Set verification checks</h2><p>At least one project check is required before run creation.</p></div>
        {mayEditSource && verification.isSuccess ? <Link className="secondary-button" to="/projects/$projectId/verification" params={{ projectId }}>{verification.data ? "Edit checks" : <><Plus size={15} aria-hidden="true" /> Add checks</>}</Link> : null}</div>
      {verification.isPending ? <div className="source-summary" role="status">Loading checks…</div> : null}
      {verification.isError ? <div className="source-summary" role="alert">Verification could not be loaded. <button type="button" className="text-action" onClick={() => void verification.refetch()}>Try again</button></div> : null}
      {verification.isSuccess ? <div className="source-summary">{verification.data ? <><strong>{verification.data.checks.length} checks · Version {verification.data.version.toString()}</strong>{verification.data.checks.map((check) => <span key={check.id}><strong>{check.id}</strong> <code>{JSON.stringify(check.command)}</code></span>)}</> : <><strong>Setup needed</strong><span>An owner or admin must add at least one verification check before creating a run.</span></>}</div> : null}
    </section> : null}
    {project.data?.project ? <section className="table-section" aria-labelledby="run-setup-heading">
      <div className="table-heading"><div><h2 id="run-setup-heading">4. Start a run</h2><p>Runs use recipe, spec, transcript, and code already committed to the selected repository.</p></div>
        {mayLaunch && launchAvailability.data?.enabled && source.data && verification.data ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> New run</Link> : null}</div>
      <div className="source-summary">
        {launchAvailability.isPending ? <span role="status">Checking run availability…</span> : null}
        {launchAvailability.isError ? <span role="alert">Run availability could not be checked. <button type="button" className="text-action" onClick={() => void launchAvailability.refetch()}>Try again</button></span> : null}
        {launchAvailability.data?.enabled ? <strong>{mayLaunch ? "Run creation is enabled" : "Run creation is available to workspace members"}</strong> : null}
        {launchAvailability.data && !launchAvailability.data.enabled ? <><strong>Run creation is not ready</strong><span>{launchAvailability.data.reason}</span></> : null}
        <span>Interactive terminal sessions and manual attachment to an AX pod are not available in this UI.</span>
      </div>
    </section> : null}
    {project.data?.project && runs.isPending ? <div className="state-panel" role="status"><RefreshCw size={22} className="spin" aria-hidden="true" /><h2>Loading runs</h2></div> : null}
    {runs.isError ? <div className="state-panel" role="alert"><h2>Runs unavailable</h2><p>Could not load runs for this project.</p><button className="secondary-button" type="button" onClick={() => void runs.refetch()}>Try again</button></div> : null}
    {runs.data && rows.length === 0 && !submittedSearch ? <section className="empty-card"><div className="empty-icon"><GitBranch size={22} aria-hidden="true" /></div><h2>No runs yet</h2><p>Once Git and verification are configured, start a run from input files already committed to the repository.</p>{mayLaunch && launchAvailability.data?.enabled && source.data && verification.data ? <Link className="secondary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Create first run</Link> : null}</section> : null}
    {rows.length > 0 || search || submittedSearch ? <section className="table-section" aria-labelledby="runs-heading"><div className="table-heading"><div><h2 id="runs-heading">Runs</h2><p>Open a run to inspect its recorded activity.</p></div><span className="fetched-time">{rows.length} loaded</span></div>
      <div className="table-toolbar"><label className="search-field"><Search size={16} aria-hidden="true" /><span className="sr-only">Search runs</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder="Search run keys or commits" maxLength={120} /></label></div>
      <DataTable table={table} label="Project runs" empty={runs.isPending || runs.isError ? undefined : "No runs match this search."} />
      <div className="table-footer"><span>{rows.length} loaded in server order</span>{runs.hasNextPage ? <button type="button" className="secondary-button" disabled={runs.isFetchingNextPage} onClick={() => void runs.fetchNextPage()}>{runs.isFetchingNextPage ? "Loading…" : "Load more"}</button> : null}</div>
    </section> : null}
  </PageShell>;
}
