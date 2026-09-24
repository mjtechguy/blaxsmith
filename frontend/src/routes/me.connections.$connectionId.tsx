import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { ConnectionDetail, LoadError, Loading, useConnections } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/me/connections/$connectionId")({ component: MyConnection });

function MyConnection() {
  const { connectionId } = Route.useParams();
  const navigate = useNavigate();
  const connections = useConnections("personal");
  const connection = connections.data?.find((c) => c.id === connectionId);
  return <PageShell>
    <PageHeader eyebrow="Personal / Connections" title="Personal connection" description="Choose the projects and models where your own runs use this connection." />
    <Link to="/me/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to my connections</Link>
    {connections.isPending ? <Loading label="Loading connection" /> : null}
    {connections.isError ? <LoadError label="Connection unavailable" retry={() => void connections.refetch()} /> : null}
    {connections.data && !connection ? <div className="state-panel" role="alert"><h2>Connection not found</h2><p>It may have been revoked.</p></div> : null}
    {connection ? <ConnectionDetail connection={connection} scope="personal" onRevoked={() => void navigate({ to: "/me/connections" })} /> : null}
  </PageShell>;
}
