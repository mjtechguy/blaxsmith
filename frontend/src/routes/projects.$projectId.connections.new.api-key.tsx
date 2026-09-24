import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { NewApiKey } from "../connection-ui";

export const Route = createFileRoute("/projects/$projectId/connections/new/api-key")({ component: ProjectNewApiKey });

function ProjectNewApiKey() {
  const { projectId } = Route.useParams();
  const navigate = useNavigate();
  return <NewApiKey scope="project" projectId={projectId} cancel={<Link to="/projects/$projectId/connections" params={{ projectId }} className="secondary-button">Cancel</Link>}
    flow={{ title: "Add a project API key", description: "Owned by this project; only its runs can use it.", back: { href: `/projects/${projectId}/connections`, label: "Project connections" } }}
    onDone={(c) => void navigate({ to: "/projects/$projectId/connections/$connectionId", params: { projectId, connectionId: c.id }, search: { tab: "usage" } as never })} />;
}
