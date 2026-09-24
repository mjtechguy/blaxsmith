import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { NewApiKey } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/projects/$projectId/connections/new/api-key")({ component: ProjectNewApiKey });

function ProjectNewApiKey() {
  const { projectId } = Route.useParams();
  const navigate = useNavigate();
  return <PageShell>
    <PageHeader eyebrow="Project / Connections" title="Add a project API key" description="Owned by this project; only its runs can use it." />
    <Link to="/projects/$projectId/connections" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    <NewApiKey scope="project" projectId={projectId} cancel={<Link to="/projects/$projectId/connections" params={{ projectId }} className="secondary-button">Cancel</Link>}
      onDone={(c) => void navigate({ to: "/projects/$projectId/connections/$connectionId", params: { projectId, connectionId: c.id } })} />
  </PageShell>;
}
