import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { ConnectionDetailPage } from "../connection-pages";
import { useConnections } from "../connection-ui";
import { NotFoundPage } from "../page";
import { StatePanel } from "../ui";

export const Route = createFileRoute("/admin/connections/$connectionId")({ component: AdminConnection });

function AdminConnection() {
  const { connectionId } = Route.useParams();
  const navigate = useNavigate();
  const connections = useConnections("organization");
  const connection = connections.data?.find((c) => c.id === connectionId);
  if (connections.isPending) return <StatePanel kind="loading" title="Loading connection" />;
  if (connections.isError) return <StatePanel kind="error" title="Connection unavailable" retry={() => void connections.refetch()} />;
  if (!connection) return <NotFoundPage title="Connection not found" back={{ to: "/admin/connections", label: "Connections" }}>It may have been revoked or belong to another scope.</NotFoundPage>;
  return <ConnectionDetailPage connection={connection} scope="organization" back={{ href: "/admin/connections", label: "Organization connections" }}
    onRevoked={() => void navigate({ to: "/admin/connections" })} />;
}
