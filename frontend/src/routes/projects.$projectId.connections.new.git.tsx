import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { NewGit, RequireProjectAdmin } from "../connection-ui";

export const Route = createFileRoute("/projects/$projectId/connections/new/git")({ component: ProjectNewGit });

function ProjectNewGit() {
  const { projectId } = Route.useParams();
  const navigate = useNavigate();
  return <RequireProjectAdmin projectId={projectId}>
    <NewGit scope="project" projectId={projectId} returnTo={`/projects/${projectId}/connections`}
      cancel={<Link to="/projects/$projectId/connections" params={{ projectId }} className="secondary-button">Cancel</Link>}
      flow={{ title: "Connect Git", description: "A GitHub or GitLab account owned by this project.", back: { href: `/projects/${projectId}/connections`, label: "Project connections" } }}
      onDone={() => void navigate({ to: "/projects/$projectId/settings/source", params: { projectId } })} />
  </RequireProjectAdmin>;
}
