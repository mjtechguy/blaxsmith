import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ShieldCheck } from "lucide-react";
import { getSessionPolicy, lifetimeLabel, sessionPolicyKey } from "../admin";
import { useOrg } from "../connection-ui";
import { StatePanel } from "../ui";

export const Route = createFileRoute("/admin/settings/sessions")({ component: SessionSettings });

// Read-only: the lifetime is platform configuration (serve-app flags, Helm
// values session.idleTimeout and session.absoluteLifetime), not per organization.
function SessionSettings() {
  const { org } = useOrg();
  const policy = useQuery({ queryKey: sessionPolicyKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getSessionPolicy(signal) });
  if (policy.isPending) return <StatePanel kind="loading" title="Loading session policy" />;
  if (policy.isError) return <StatePanel kind="error" title="Session policy unavailable" retry={() => void policy.refetch()} />;
  const p = policy.data;
  const rows = [
    { id: "idle", title: "Idle timeout", value: lifetimeLabel(p.idleTimeoutSeconds), text: "A browser session ends after this long without activity. Each silent renewal moves it forward." },
    { id: "absolute", title: "Absolute lifetime", value: lifetimeLabel(p.absoluteLifetimeSeconds), text: "Every session ends this long after sign-in, however active it is. People then sign in again." },
    { id: "access", title: "Access token", value: lifetimeLabel(p.accessTokenSeconds), text: "Short-lived and renewed in the background through an HttpOnly cookie. Server restarts and rollouts do not sign anyone out." },
    { id: "grace", title: "Refresh grace window", value: lifetimeLabel(p.refreshGraceSeconds), text: "Tabs renewing at the same moment share one result. Reusing an older refresh token after this window signs that session out everywhere." },
  ];
  return <section className="editor-card" aria-labelledby="session-policy-heading">
    <div className="editor-card-heading"><span className="project-symbol"><ShieldCheck size={18} aria-hidden="true" /></span><div>
      <h2 id="session-policy-heading">Sessions</h2>
      <p>How long browser sessions last. Set by the platform operator in the deployment; shown here read-only.</p></div></div>
    <ul className="flag-list">
      {rows.map((row) => <li key={row.id} className="flag-row"><div>
        <span className="flag-title">{row.title}</span>
        <p>{row.text}</p>
      </div><strong>{row.value}</strong></li>)}
    </ul>
    <p className="card-note">People review and sign out their own sessions under <Link to="/me/settings/sessions" className="text-action">Account settings › Sessions</Link>. To sign a member out of every session, use Sign out everywhere under <Link to="/admin/users" className="text-action">Users</Link>.</p>
  </section>;
}
