import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConnectionDetailPage } from "../connection-pages";
import { useConnections } from "../connection-ui";
import { StatePanel } from "../ui";

export const Route = createFileRoute("/projects/$projectId/connections/$connectionId")({ component: ProjectConnection });

function ProjectConnection() {
  const { projectId, connectionId } = Route.useParams();
  const navigate = useNavigate();
  const owned = useConnections("project", projectId);
  const available = useConnections("project_available", projectId);
  const connection = owned.data?.find((c) => c.id === connectionId) ?? available.data?.find((c) => c.id === connectionId);
  if (owned.isPending || available.isPending) return <StatePanel kind="loading" title="Loading connection" />;
  if (owned.isError || available.isError) return <StatePanel kind="error" title="Connection unavailable" retry={() => { void owned.refetch(); void available.refetch(); }} />;
  if (!connection) return <StatePanel kind="error" title="Connection not available">It is not owned by or granted to this project.</StatePanel>;
  return <ConnectionDetailPage connection={connection} scope="project" projectId={projectId} back={{ href: `/projects/${projectId}/connections`, label: "Project connections" }}
    onRevoked={() => void navigate({ to: "/projects/$projectId/connections", params: { projectId } })} />;
}
