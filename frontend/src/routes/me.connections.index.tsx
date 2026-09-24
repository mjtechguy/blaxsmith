import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, KeyRound, UserRound } from "lucide-react";
import { ConnectionTable, LoadError, Loading, useConnections } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/me/connections/")({ component: MyConnections });

function MyConnections() {
  const connections = useConnections("personal");
  return <PageShell>
    <PageHeader eyebrow="Personal" title="My connections" description="Your own subscriptions and API keys. They serve only runs you launch; nobody else can use them."
      actions={<>
        <Link className="primary-button" to="/me/connections/new/subscription"><UserRound size={15} aria-hidden="true" /> Subscription</Link>
        <Link className="secondary-button" to="/me/connections/new/api-key"><KeyRound size={15} aria-hidden="true" /> API key</Link>
      </>} />
    {connections.isPending ? <Loading label="Loading your connections" /> : null}
    {connections.isError ? <LoadError label="Your connections are unavailable" retry={() => void connections.refetch()} /> : null}
    {connections.data ? <section className="table-section" aria-labelledby="my-connections-heading">
      <div className="table-heading"><div><h2 id="my-connections-heading">Personal connections</h2><p>Use a connection in a project to let your runs there pick its models.</p></div><span className="fetched-time">{connections.data.length} connections</span></div>
      <ConnectionTable connections={connections.data} label="Personal connections" empty="No personal connections yet."
        manage={(c) => <Link className="text-action" to="/me/connections/$connectionId" params={{ connectionId: c.id }}>Use in project <ArrowRight size={13} aria-hidden="true" /></Link>} />
    </section> : null}
  </PageShell>;
}
