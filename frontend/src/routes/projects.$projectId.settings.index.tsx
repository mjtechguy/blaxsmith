import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight } from "lucide-react";
import { SummaryList } from "../layouts";
import { Card, CopyValue, StatePanel, Timestamp } from "../ui";
import { useScope } from "../workspace-ui";
import { getProject } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/settings/")({ component: General });

function General() {
  const { projectId } = Route.useParams();
  const { org } = useScope();
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const p = project.data?.project;
  if (project.isPending) return <StatePanel kind="loading" title="Loading project" />;
  if (!p) return <StatePanel kind="error" title="Project unavailable" retry={() => void project.refetch()} />;
  return <>
    <Card title="General" description="How this project is identified. Names and URLs cannot be changed yet.">
      <div className="card-body"><SummaryList items={[
        { label: "Name", value: p.name },
        { label: "URL name", value: <code>{p.slug}</code> },
        { label: "Project ID", value: <CopyValue value={p.id} label="Project ID" chars={13} /> },
        { label: "Created", value: <Timestamp value={p.createdAt} /> },
      ]} /></div>
    </Card>
    <Card title="Related settings" description="Access for this project's runs is managed where it is used.">
      <ul className="link-list">
        <li><Link to="/projects/$projectId/connections" params={{ projectId }}>Connections and model access <ArrowRight size={13} aria-hidden="true" /></Link></li>
        <li><Link to="/projects/$projectId/recipes" params={{ projectId }}>Recipes available to this project <ArrowRight size={13} aria-hidden="true" /></Link></li>
      </ul>
    </Card>
  </>;
}
