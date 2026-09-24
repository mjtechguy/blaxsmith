import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft } from "lucide-react";
import { NewGit } from "../connection-ui";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/projects/$projectId/connections/new/git")({ component: ProjectNewGit });

function ProjectNewGit() {
  const { projectId } = Route.useParams();
  const navigate = useNavigate();
  return <PageShell>
    <PageHeader eyebrow="Project / Connections" title="Connect Git" description="A GitHub or GitLab account owned by this project." />
    <Link to="/projects/$projectId/connections" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to connections</Link>
    <NewGit scope="project" projectId={projectId} returnTo={`/projects/${projectId}/connections`}
      cancel={<Link to="/projects/$projectId/connections" params={{ projectId }} className="secondary-button">Cancel</Link>}
      onDone={() => void navigate({ to: "/projects/$projectId/source", params: { projectId } })} />
  </PageShell>;
}
