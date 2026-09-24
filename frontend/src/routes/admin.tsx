import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, Outlet } from "@tanstack/react-router";
import { ShieldAlert } from "lucide-react";
import { isOrgAdmin } from "../admin";
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
  const allowed = session.data !== undefined ? isOrgAdmin(session.data) : adminAllowed;
  if (!allowed) return <AdminRestricted />;
  // Section navigation lives in the sidebar's Admin group.
  return <Outlet />;
}
