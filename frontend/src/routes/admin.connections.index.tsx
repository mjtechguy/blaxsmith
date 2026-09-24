import { createFileRoute, Link, useLocation } from "@tanstack/react-router";
import { ArrowLeft, ArrowRight, GitBranch, KeyRound, Plus, Settings, ShieldAlert, UserRound } from "lucide-react";
import { isOrgAdmin } from "../admin";
import { ConnectionTable, GitHubReturnNotice, LoadError, Loading, useConnections, useOrg } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/admin/connections/")({ component: AdminConnections });

function AdminConnections() {
  const { session } = useOrg();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const allowed = isOrgAdmin(session.data);
  const connections = useConnections("organization", "", allowed);
  if (session.data && !allowed) return <PageShell><div className="state-panel" role="alert"><ShieldAlert size={22} aria-hidden="true" /><h2>Administration is restricted</h2><p>Only organization owners and admins can manage organization connections.</p><Link className="secondary-button" to="/me/connections">My connections</Link></div></PageShell>;
  return <PageShell>
    <PageHeader eyebrow="Administration" title="Connections" description="Organization-owned model keys, Git access, and their grants to projects, users, and roles. Secrets are write-only."
      actions={<>
        <Link className="primary-button" to="/admin/connections/new/api-key"><KeyRound size={15} aria-hidden="true" /> API key</Link>
        <Link className="secondary-button" to="/admin/connections/new/git"><GitBranch size={15} aria-hidden="true" /> Git</Link>
        <Link className="secondary-button" to="/me/connections/new/subscription"><UserRound size={15} aria-hidden="true" /> Subscription</Link>
        <Link className="secondary-button" to="/admin/connections/github-app"><Settings size={15} aria-hidden="true" /> GitHub App</Link>
      </>} />
    <Link to="/admin" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Operations</Link>
    <GitHubReturnNotice search={search} />
    {connections.isPending ? <Loading label="Loading connections" /> : null}
    {connections.isError ? <LoadError label="Connections unavailable" retry={() => void connections.refetch()} /> : null}
    {connections.data ? <section className="table-section" aria-labelledby="org-connections-heading">
      <div className="table-heading"><div><h2 id="org-connections-heading">Organization connections</h2><p>Subscriptions are always personal; they appear under My connections.</p></div><span className="fetched-time">{connections.data.length} connections</span></div>
      <ConnectionTable connections={connections.data} label="Organization connections" empty="No organization connections yet. Use + API key or + Git."
        manage={(c) => <Link className="text-action" to="/admin/connections/$connectionId" params={{ connectionId: c.id }}>Manage <ArrowRight size={13} aria-hidden="true" /></Link>} />
      {connections.data.length === 0 ? <Link className="secondary-button" to="/admin/connections/new/api-key"><Plus size={15} aria-hidden="true" /> Add the first key</Link> : null}
    </section> : null}
  </PageShell>;
}
