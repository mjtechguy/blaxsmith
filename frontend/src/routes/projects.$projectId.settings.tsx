import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Outlet, useLocation } from "@tanstack/react-router";
import { SettingsLayout } from "../layouts";
import { useScope } from "../workspace-ui";
import { getProject } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/settings")({ component: ProjectSettings });

function ProjectSettings() {
  const { projectId } = Route.useParams();
  const { org } = useScope();
  const pathname = useLocation({ select: (l) => l.pathname.replace(/\/+$/, "") });
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const base = `/projects/${projectId}/settings`;
  const current = pathname.endsWith("/source") ? "source" : pathname.endsWith("/verification") ? "verification" : "general";
  return <SettingsLayout title="Settings" description={project.data?.project ? `Configuration for ${project.data.project.name}. Each section saves on its own.` : "Project configuration."} current={current}
    sections={[
      { id: "general", label: "General", href: base },
      { id: "source", label: "Git source", href: `${base}/source` },
      { id: "verification", label: "Verification", href: `${base}/verification` },
    ]}>
    <Outlet />
  </SettingsLayout>;
}
