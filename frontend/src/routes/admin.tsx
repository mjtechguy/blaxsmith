import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet, useLocation } from "@tanstack/react-router";
import { ShieldAlert } from "lucide-react";
import { adminSectionFor, adminSections, isOrgAdmin } from "../admin";
import { currentSession, sessionQueryKey } from "../auth";
import { PageShell } from "../page";

// One guarded section: every /admin/* page is a child of this layout. The
// server still enforces owner/admin on each admin RPC; this keeps members out
// of admin pages (and their queries) instead of each page checking alone.
export const Route = createFileRoute("/admin")({
  beforeLoad: async ({ context }) => {
    const session = await context.queryClient.ensureQueryData({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) })
      .catch(() => null);
    return { adminAllowed: isOrgAdmin(session) };
  },
  component: AdminLayout,
});

function AdminRestricted() {
  return <PageShell><div className="state-panel" role="alert"><ShieldAlert size={22} aria-hidden="true" /><h2>Administration is restricted</h2>
    <p>Only organization owners and admins can open administration. Your own connections stay under My connections.</p>
    <span className="admin-actions"><Link className="secondary-button" to="/">Back to workspace</Link><Link className="secondary-button" to="/me/connections">My connections</Link></span></div></PageShell>;
}

function AdminLayout() {
  const { adminAllowed } = Route.useRouteContext();
  // The live session wins over the value checked at navigation time (role changes, sign-out in another tab).
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const pathname = useLocation({ select: (location) => location.pathname });
  const allowed = session.data !== undefined ? isOrgAdmin(session.data) : adminAllowed;
  if (!allowed) return <AdminRestricted />;
  const current = adminSectionFor(pathname);
  return <div className="admin-section">
    <nav className="admin-subnav" aria-label="Administration">
      {adminSections.map((s) => <Link key={s.to} to={s.to} activeOptions={{ exact: true }} className="admin-subnav-link" aria-current={s.to === current ? "page" : undefined}>{s.label}</Link>)}
    </nav>
    <Outlet />
  </div>;
}
