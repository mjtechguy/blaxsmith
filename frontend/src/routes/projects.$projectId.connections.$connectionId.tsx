import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConnectionDetailPage } from "../connection-pages";
import { useConnections, useProjectAccess } from "../connection-ui";
import { NotFoundPage } from "../page";
import { StatePanel } from "../ui";

export const Route = createFileRoute("/projects/$projectId/connections/$connectionId")({ component: ProjectConnection });

function ProjectConnection() {
  const { projectId, connectionId } = Route.useParams();
  const navigate = useNavigate();
  // Only project admins may list the project's own connections; everyone
  // else reaches granted organization connections here.
  const { project, canAdminister } = useProjectAccess(projectId);
  const owned = useConnections("project", projectId, canAdminister);
  const available = useConnections("project_available", projectId);
  const connection = owned.data?.find((c) => c.id === connectionId) ?? available.data?.find((c) => c.id === connectionId);
  if (project.isPending || (canAdminister && owned.isPending) || available.isPending) return <StatePanel kind="loading" title="Loading connection" />;
  if (!connection && (owned.isError || available.isError)) return <StatePanel kind="error" title="Connection unavailable" retry={() => { void owned.refetch(); void available.refetch(); }} />;
  if (!connection) return <NotFoundPage title="Connection not found" back={{ to: `/projects/${projectId}/connections`, label: "Project connections" }}>
    It is not owned by or granted to this project, or it was revoked.</NotFoundPage>;
  return <ConnectionDetailPage connection={connection} scope="project" projectId={projectId} canAdminister={canAdminister} back={{ href: `/projects/${projectId}/connections`, label: "Project connections" }}
    onRevoked={() => void navigate({ to: "/projects/$projectId/connections", params: { projectId } })} />;
}
