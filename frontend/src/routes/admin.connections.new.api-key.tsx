import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { NewApiKey } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/admin/connections/new/api-key")({ component: AdminNewApiKey });

function AdminNewApiKey() {
  const navigate = useNavigate();
  return <PageShell>
    <PageHeader eyebrow="Administration / Connections" title="Add an organization API key" description="Organization-owned; grant it to projects, users, or roles afterwards." />
    <Link to="/admin/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    <NewApiKey scope="organization" cancel={<Link to="/admin/connections" className="secondary-button">Cancel</Link>}
      onDone={(c) => void navigate({ to: "/admin/connections/$connectionId", params: { connectionId: c.id } })} />
  </PageShell>;
}
