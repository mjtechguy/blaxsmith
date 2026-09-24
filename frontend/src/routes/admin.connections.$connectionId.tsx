import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { ConnectionDetail, LoadError, Loading, useConnections } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/admin/connections/$connectionId")({ component: AdminConnection });

function AdminConnection() {
  const { connectionId } = Route.useParams();
  const navigate = useNavigate();
  const connections = useConnections("organization");
  const connection = connections.data?.find((c) => c.id === connectionId);
  return <PageShell>
    <PageHeader eyebrow="Administration / Connections" title="Manage connection" description="Grants, model uses, and model discovery for one organization connection." />
    <Link to="/admin/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    {connections.isPending ? <Loading label="Loading connection" /> : null}
    {connections.isError ? <LoadError label="Connection unavailable" retry={() => void connections.refetch()} /> : null}
    {connections.data && !connection ? <div className="state-panel" role="alert"><h2>Connection not found</h2><p>It may have been revoked or belong to another scope.</p></div> : null}
    {connection ? <ConnectionDetail connection={connection} scope="organization" onRevoked={() => void navigate({ to: "/admin/connections" })} /> : null}
  </PageShell>;
}
