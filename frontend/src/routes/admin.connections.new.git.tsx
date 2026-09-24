import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { NewGit } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/admin/connections/new/git")({ component: AdminNewGit });

function AdminNewGit() {
  const navigate = useNavigate();
  return <PageShell>
    <PageHeader eyebrow="Administration / Connections" title="Connect Git" description="An organization GitHub or GitLab account for private sources and run-branch pushes." />
    <Link to="/admin/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    <NewGit scope="organization" returnTo="/admin/connections" cancel={<Link to="/admin/connections" className="secondary-button">Cancel</Link>}
      onDone={(c) => void navigate({ to: "/admin/connections/$connectionId", params: { connectionId: c.id } })} />
  </PageShell>;
}
