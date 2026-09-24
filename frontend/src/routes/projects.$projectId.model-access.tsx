import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useMatchRoute } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowLeft, KeyRound, Plus, RefreshCw } from "lucide-react";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { ProjectModelAccess } from "../gen/blaxsmith/api/v1/workflow_pb";
import { PageHeader, PageShell } from "../page";
import { getProject, listProjectModelAccess, projectModelAccessQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/model-access")({ component: ModelAccess });

const features = tableFeatures({});
const columns: ColumnDef<typeof features, ProjectModelAccess>[] = [
  { id: "provider", accessorKey: "provider", header: "Provider", cell: ({ row }) => <strong>{row.original.provider === "openai" ? "OpenAI" : row.original.provider === "anthropic" ? "Anthropic" : row.original.provider}</strong> },
  { id: "model", accessorKey: "model", header: "Model", cell: ({ row }) => <span className="mono">{row.original.model}</span> },
  { id: "connection", accessorKey: "connectionId", header: "Connection", cell: ({ row }) => <span className={`state-badge ${row.original.connectionId ? "state-active" : "state-blocked"}`}>{row.original.connectionId ? "Configured" : "Unavailable"}</span> },
  { id: "grant", accessorKey: "grantId", header: "Grant", cell: ({ row }) => <span className={`state-badge ${row.original.grantId ? "state-active" : "state-blocked"}`}>{row.original.grantId ? "Recorded" : "Unavailable"}</span> },
  { id: "created", accessorKey: "createdAt", header: "Added", cell: ({ row }) => <time dateTime={row.original.createdAt}>{new Date(row.original.createdAt).toLocaleString()}</time> },
];

function ModelAccess() {
  const { projectId } = Route.useParams();
  const adding = useMatchRoute()({ to: "/projects/$projectId/model-access/new" });
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org && !adding), queryFn: ({ signal }) => getProject(projectId, signal) });
  const access = useQuery({ queryKey: projectModelAccessQueryKey(org, projectId), enabled: Boolean(org && !adding && project.data?.project), queryFn: ({ signal }) => listProjectModelAccess(projectId, signal) });
  const mayAdd = session.data?.role === "owner" || session.data?.role === "admin";
  const table = useTable({ features, data: access.data?.access || [], columns, getRowId: (entry) => entry.id });

  if (adding) return <Outlet />;

  return <PageShell>
    <PageHeader eyebrow="Project / Model access" title="Model access" description={project.data?.project ? `Model connections granted to ${project.data.project.name}.` : "Model connections granted to this project."}
      actions={mayAdd && project.data?.project ? <Link className="primary-button" to="/projects/$projectId/model-access/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add model access</Link> : undefined} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to project</Link>
    {project.isPending || (project.isSuccess && access.isPending) ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading model access</h2></div> : null}
    {project.isError ? <div className="state-panel" role="alert"><h2>Project unavailable</h2><p>This project could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void project.refetch()}>Try again</button></div> : null}
    {access.isError ? <div className="state-panel" role="alert"><h2>Model access unavailable</h2><p>Connections and grants could not be loaded.</p><button className="secondary-button" type="button" onClick={() => void access.refetch()}>Try again</button></div> : null}
    {project.data?.project && access.isSuccess && access.data.access.length === 0 ? <section className="empty-card"><span className="empty-icon"><KeyRound size={22} aria-hidden="true" /></span><h2>No model access yet</h2><p>An organization owner or admin can add a provider key and grant a model to this project.</p>{mayAdd ? <Link className="secondary-button" to="/projects/$projectId/model-access/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add model access</Link> : null}</section> : null}
    {project.data?.project && access.isSuccess && access.data.access.length > 0 ? <section className="table-section" aria-labelledby="model-access-table-heading">
      <div className="table-heading"><div><h2 id="model-access-table-heading">Project grants</h2><p>Configured connections and recorded grants. Provider key validity is not checked here.</p></div><span className="fetched-time">{access.data.access.length} configured</span></div>
      <DataTable table={table} label="Project model access" />
    </section> : null}
  </PageShell>;
}
