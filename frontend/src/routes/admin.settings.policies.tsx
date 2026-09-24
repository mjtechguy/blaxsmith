import { createFileRoute } from "@tanstack/react-router";

export const Route = createFileRoute("/admin/settings/policies")({ component: Policies });

// Placeholder: no policy engine exists yet, so there is no form to fill.
function Policies() {
  return <section className="coming-soon" aria-labelledby="policies-heading">
    <span className="soon-badge">Coming soon</span>
    <h2 id="policies-heading">Policies</h2>
    <p>Organization-wide rules for runs, such as which harnesses and models projects may use and which stages need a human decision. Nothing here is configurable yet; today these limits come from recipes, grants, and roles.</p>
  </section>;
}
