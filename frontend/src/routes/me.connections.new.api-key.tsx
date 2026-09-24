import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { NewApiKey } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/me/connections/new/api-key")({ component: MyNewApiKey });

function MyNewApiKey() {
  const navigate = useNavigate();
  return <PageShell>
    <PageHeader eyebrow="Personal / Connections" title="Add a personal API key" description="Yours only; it serves runs you launch." />
    <Link to="/me/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to my connections</Link>
    <NewApiKey scope="personal" cancel={<Link to="/me/connections" className="secondary-button">Cancel</Link>}
      onDone={(c) => void navigate({ to: "/me/connections/$connectionId", params: { connectionId: c.id } })} />
  </PageShell>;
}
