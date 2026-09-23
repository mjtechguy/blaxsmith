import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, FolderKanban } from "lucide-react";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/")({ component: Workspace });

function Workspace() {
  return <PageShell>
    <PageHeader eyebrow="Workspace" title="Engineering work" description="Projects, conversations, evidence, and final human reviews will live here." />
    <section className="empty-card" aria-labelledby="workspace-empty-title">
      <div className="empty-icon"><FolderKanban size={22} aria-hidden="true" /></div>
      <h2 id="workspace-empty-title">No projects are connected yet</h2>
      <p>The application identity, project API, and AX scheduler are still being built. The runtime catalog is available now for review.</p>
      <Link to="/tools" className="text-action">Explore runtime releases <ArrowRight size={15} aria-hidden="true" /></Link>
    </section>
  </PageShell>;
}
