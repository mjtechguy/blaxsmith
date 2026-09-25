import { createFileRoute, Link } from "@tanstack/react-router";
import { Cable, Plus } from "lucide-react";
import { ConnectionCollection } from "../connection-pages";
import { LoadError, Loading, useConnections } from "../connection-ui";
import { PageHeader, PageShell } from "../page";
import { EmptyState } from "../ui";

export const Route = createFileRoute("/me/connections/")({ component: MyConnections });

function MyConnections() {
  const connections = useConnections("personal");
  const actions = <>
    <Link className="primary-button" to="/me/connections/new/subscription"><Plus size={15} aria-hidden="true" /> Add subscription</Link>
    <Link className="secondary-button" to="/me/connections/new/api-key"><Plus size={15} aria-hidden="true" /> Add API key</Link>
  </>;
  return <PageShell>
    <PageHeader title="My connections" description="Your own subscriptions and API keys. They serve only runs you launch; nobody else can use them." actions={actions} />
    {connections.isPending ? <Loading label="Loading your connections" /> : null}
    {connections.isError ? <LoadError label="Your connections are unavailable" retry={() => void connections.refetch()} /> : null}
    {connections.data ? <section className="table-section" aria-label="Personal connections">
      <ConnectionCollection id="my-connections" label="Personal connections" connections={connections.data} urlState action="Use in project" href={(c) => `/me/connections/${c.id}`}
        empty={<EmptyState icon={<Cable size={22} aria-hidden="true" />} title="No personal connections yet" action={<div className="page-actions">{actions}</div>}>Add a subscription or key, then choose the projects where your runs use it.</EmptyState>} />
    </section> : null}
  </PageShell>;
}
