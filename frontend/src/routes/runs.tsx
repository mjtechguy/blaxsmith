import { createFileRoute, Link } from "@tanstack/react-router";
import { PageHeader, PageShell } from "../page";
import { NoRuns, RunsCollection } from "../runs-view";

export const Route = createFileRoute("/runs")({ component: Runs });

function Runs() {
  return <PageShell>
    <PageHeader title="Runs" description="Every run in your organization. Expand a row to see its stages; open it for the full record." />
    <section className="table-section" aria-label="All runs">
      <RunsCollection empty={<NoRuns>Start a run from a project. <Link className="text-action" to="/projects">Choose a project</Link></NoRuns>} />
    </section>
  </PageShell>;
}
