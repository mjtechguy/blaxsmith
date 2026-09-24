import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/admin/settings/retention")({ component: Retention });

// Placeholder: retention windows are not implemented, so nothing is editable here.
function Retention() {
  return <section className="coming-soon" aria-labelledby="retention-heading">
    <span className="soon-badge">Coming soon</span>
    <h2 id="retention-heading">Retention</h2>
    <p>How long run activity, work logs, and audit events are kept. Nothing is deleted automatically today, and there is no setting to change yet.</p>
  </section>;
}
