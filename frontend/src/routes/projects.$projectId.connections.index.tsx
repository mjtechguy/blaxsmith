import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, useLocation } from "@tanstack/react-router";
import { ArrowLeft, ArrowRight, GitBranch, KeyRound, UserRound } from "lucide-react";
import { ConnectionTable, GitHubReturnNotice, LoadError, Loading, useConnections, useOrg } from "../connection-ui";
import { PageHeader, PageShell } from "../page";
import { getProject } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/connections/")({ component: ProjectConnections });

function ProjectConnections() {
  const { projectId } = Route.useParams();
  const { org } = useOrg();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const owned = useConnections("project", projectId, Boolean(project.data?.project));
  const available = useConnections("project_available", projectId, Boolean(project.data?.project));
  const manage = (label: string) => (c: { id: string }) => <Link className="text-action" to="/projects/$projectId/connections/$connectionId" params={{ projectId, connectionId: c.id }}>{label} <ArrowRight size={13} aria-hidden="true" /></Link>;
  return <PageShell>
    <PageHeader eyebrow="Project / Connections" title="Connections" description={project.data?.project ? `Model and Git access for ${project.data.project.name}.` : "Model and Git access for this project."}
      actions={<>
        <Link className="primary-button" to="/projects/$projectId/connections/new/api-key" params={{ projectId }}><KeyRound size={15} aria-hidden="true" /> API key</Link>
        <Link className="secondary-button" to="/projects/$projectId/connections/new/git" params={{ projectId }}><GitBranch size={15} aria-hidden="true" /> Git</Link>
        <Link className="secondary-button" to="/me/connections/new/subscription"><UserRound size={15} aria-hidden="true" /> Subscription</Link>
      </>} />
    <Link to="/projects/$projectId" params={{ projectId }} className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to project</Link>
    <GitHubReturnNotice search={search} />
    {project.isPending || owned.isPending ? <Loading label="Loading connections" /> : null}
    {project.isError ? <LoadError label="Project unavailable" retry={() => void project.refetch()} /> : null}
    {owned.isError ? <LoadError label="Project connections unavailable" retry={() => void owned.refetch()} /> : null}
    {owned.data ? <section className="table-section" aria-labelledby="project-connections-heading">
      <div className="table-heading"><div><h2 id="project-connections-heading">Project connections</h2><p>Owned by this project and usable only by its runs. Project owners and admins manage them.</p></div><span className="fetched-time">{owned.data.length} connections</span></div>
      <ConnectionTable connections={owned.data} label="Project connections" empty="No project-owned connections." manage={manage("Manage")} />
    </section> : null}
    {available.isError ? <LoadError label="Organization connections unavailable" retry={() => void available.refetch()} /> : null}
    {available.data ? <section className="table-section" aria-labelledby="available-connections-heading">
      <div className="table-heading"><div><h2 id="available-connections-heading">Use an organization connection</h2><p>Only organization connections granted to this project are listed. Pick a model to attach one.</p></div><span className="fetched-time">{available.data.length} granted</span></div>
      <ConnectionTable connections={available.data} label="Granted organization connections" empty="No organization connections are granted to this project. Ask an organization admin." manage={manage("Use")} explainIn={projectId} />
    </section> : null}
    <p className="admin-note">Personal subscriptions and keys are added under <Link to="/me/connections" className="text-action">My connections</Link> and serve only runs their owner launches.</p>
  </PageShell>;
}
