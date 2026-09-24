import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { ConnectionDetail, LoadError, Loading, useConnections } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/projects/$projectId/connections/$connectionId")({ component: ProjectConnection });

function ProjectConnection() {
  const { projectId, connectionId } = Route.useParams();
  const navigate = useNavigate();
  const owned = useConnections("project", projectId);
  const available = useConnections("project_available", projectId);
  const connection = owned.data?.find((c) => c.id === connectionId) ?? available.data?.find((c) => c.id === connectionId);
  const pending = owned.isPending || available.isPending;
  return <PageShell>
    <PageHeader eyebrow="Project / Connections" title="Connection" description="Attach models from this connection to the project's runs." />
    <Link to="/projects/$projectId/connections" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    {pending ? <Loading label="Loading connection" /> : null}
    {owned.isError || available.isError ? <LoadError label="Connection unavailable" retry={() => { void owned.refetch(); void available.refetch(); }} /> : null}
    {!pending && !connection && !owned.isError && !available.isError ? <div className="state-panel" role="alert"><h2>Connection not available</h2><p>It is not owned by or granted to this project.</p></div> : null}
    {connection ? <ConnectionDetail connection={connection} scope="project" projectId={projectId}
      onRevoked={() => void navigate({ to: "/projects/$projectId/connections", params: { projectId } })} /> : null}
  </PageShell>;
}
