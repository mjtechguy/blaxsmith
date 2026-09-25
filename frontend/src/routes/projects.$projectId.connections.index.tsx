import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, useLocation } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { ConnectionCollection } from "../connection-pages";
import { GitHubReturnNotice, LoadError, Loading, useConnections, useOrg } from "../connection-ui";
import { isMissing, NotFoundPage, PageHeader, PageShell } from "../page";
import { EmptyState } from "../ui";
import { getProject } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/connections/")({ component: ProjectConnections });

function ProjectConnections() {
  const { projectId } = Route.useParams();
  const { org } = useOrg();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  // The server says who administers this project (org owners and admins, or
  // its creator). Only they may list or add its own connections.
  const canAdminister = Boolean(project.data?.canAdminister);
  const owned = useConnections("project", projectId, canAdminister);
  const available = useConnections("project_available", projectId, Boolean(project.data?.project));
  const href = (c: { id: string }) => `/projects/${projectId}/connections/${c.id}`;
  const actions = !canAdminister ? undefined : <>
    <Link className="primary-button" to="/projects/$projectId/connections/new/api-key" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add API key</Link>
    <Link className="secondary-button" to="/projects/$projectId/connections/new/git" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Add Git</Link>
  </>;
  if (project.isError && isMissing(project.error)) return <NotFoundPage title="Project not found" back={{ to: "/projects", label: "All projects" }}>
    This project does not exist or is not in your organization.</NotFoundPage>;
  return <PageShell>
    <PageHeader title="Project connections" description={project.data?.project ? `Model and Git access for ${project.data.project.name}.` : "Model and Git access for this project."}
      actions={actions} />
    <GitHubReturnNotice search={search} />
    {project.isPending || (canAdminister && owned.isPending) ? <Loading label="Loading connections" /> : null}
    {project.isError ? <LoadError label="Project unavailable" retry={() => void project.refetch()} /> : null}
    {owned.isError ? <LoadError label="Project connections unavailable" retry={() => void owned.refetch()} /> : null}
    {project.data && !canAdminister ? <p className="notice" role="note"><strong>Project-owned keys are managed by project admins.</strong> Runs here still use them; you see the organization connections granted to this project below.</p> : null}
    {owned.data ? <section className="table-section" aria-labelledby="project-connections-heading">
      <div className="table-heading"><div><h2 id="project-connections-heading">Project connections</h2><p>Owned by this project and usable only by its runs. You administer this project, so you can add and manage them.</p></div></div>
      <ConnectionCollection id="project-connections" label="Project connections" connections={owned.data} urlState href={href}
        empty={<EmptyState title="No project connections" action={actions ? <div className="page-actions">{actions}</div> : undefined}>Add a key that only this project’s runs can use.</EmptyState>} />
    </section> : null}
    {available.isError ? <LoadError label="Organization connections unavailable" retry={() => void available.refetch()} /> : null}
    {available.data ? <section className="table-section" aria-labelledby="available-connections-heading">
      <div className="table-heading"><div><h2 id="available-connections-heading">Granted organization connections</h2><p>{canAdminister ? "Organization connections granted to this project. Open one to attach a model." : "Organization connections granted to this project. Project admins attach their models."}</p></div></div>
      <ConnectionCollection id="project-granted-connections" label="Granted organization connections" connections={available.data} action={canAdminister ? "Use" : "View"} href={href} explainIn={projectId}
        empty={<EmptyState title="Nothing is granted to this project">Ask an organization admin to grant a connection.</EmptyState>} />
    </section> : null}
    <p className="page-footnote">Personal subscriptions and keys live in <Link to="/me/connections" className="text-action">My connections</Link> and serve only runs their owner launches.</p>
  </PageShell>;
}
