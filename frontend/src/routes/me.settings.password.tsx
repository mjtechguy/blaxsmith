import { createFileRoute } from "@tanstack/react-router";
import { accountActions } from "../account";

export const Route = createFileRoute("/me/settings/password")({ component: Password });

function Password() {
  if (accountActions) return null;
  return <section className="coming-soon" aria-labelledby="password-soon">
    <span className="soon-badge">Coming soon</span>
    <h2 id="password-soon">Change password</h2>
    <p>Changing your password from here is not available yet. Until it is, an organization owner or admin can issue you a one-time reset link from Admin › Users; using it sets a new password and signs you out everywhere.</p>
  </section>;
}
