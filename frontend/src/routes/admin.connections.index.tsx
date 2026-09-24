import { createFileRoute, Link, useLocation } from "@tanstack/react-router";
import { KeyRound, Plus } from "lucide-react";
import { ConnectionCollection } from "../connection-pages";
import { GitHubReturnNotice, LoadError, Loading, useConnections } from "../connection-ui";
import { PageHeader, PageShell } from "../page";
import { EmptyState } from "../ui";

export const Route = createFileRoute("/admin/connections/")({ component: AdminConnections });

function AdminConnections() {
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const connections = useConnections("organization");
  const add = <Link className="primary-button" to="/admin/connections/new"><Plus size={15} aria-hidden="true" /> Add connection</Link>;
  return <PageShell>
    <PageHeader title="Connections" description="Organization-owned model keys and Git access, and their grants to projects, users, and roles. Secrets are write-only." actions={add} />
    <GitHubReturnNotice search={search} />
    {connections.isPending ? <Loading label="Loading connections" /> : null}
    {connections.isError ? <LoadError label="Connections unavailable" retry={() => void connections.refetch()} /> : null}
    {connections.data ? <section className="table-section" aria-label="Organization connections">
      <ConnectionCollection id="admin-connections" label="Organization connections" connections={connections.data} urlState href={(c) => `/admin/connections/${c.id}`}
        empty={<EmptyState icon={<KeyRound size={22} aria-hidden="true" />} title="No organization connections yet" action={add}>Add a provider API key or Git access, then grant it to projects. Subscriptions are personal and live under My connections.</EmptyState>} />
    </section> : null}
  </PageShell>;
}
