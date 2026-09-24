import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Plus } from "lucide-react";
import { PageHeader, PageShell } from "../page";
import { NoRuns, RunsCollection } from "../runs-view";
import { useScope } from "../workspace-ui";
import { getLaunchAvailability, getProject, launchAvailabilityQueryKey } from "../workflow";

export const Route = createFileRoute("/projects/$projectId/runs/")({ component: ProjectRuns });

function ProjectRuns() {
  const { projectId } = Route.useParams();
  const { org, isMember } = useScope();
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const launch = useQuery({ queryKey: launchAvailabilityQueryKey(org, projectId), enabled: Boolean(org && isMember), queryFn: ({ signal }) => getLaunchAvailability(projectId, signal) });
  const newRun = isMember && launch.data?.enabled ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> New run</Link> : undefined;
  return <PageShell>
    <PageHeader title="Runs" description={project.data?.project ? `Every run in ${project.data.project.name}.` : "Every run in this project."} actions={newRun} />
    {isMember && launch.data && !launch.data.enabled ? <p className="notice" role="note"><strong>New runs are unavailable.</strong> {launch.data.reason}</p> : null}
    <section className="table-section" aria-label="Project runs">
      <RunsCollection projectId={projectId} empty={<NoRuns>{newRun ? "Start a run from files already committed to the repository." : "Finish the project setup to start the first run."}</NoRuns>} />
    </section>
  </PageShell>;
}
