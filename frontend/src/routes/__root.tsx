import { useEffect, useRef, type ReactNode } from "react";
import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { createRootRouteWithContext, Navigate, Outlet, useLocation } from "@tanstack/react-router";
import { clearWorkspaceCache, currentSession, emailSetupPath, isAccountLinkRoute, isPublicCatalogRoute, onOtherTabSessionChange, REFRESH_AHEAD_MS, sessionQueryKey } from "../auth";
import { backoffDelay, isUnavailable, probeNow } from "../connectivity";
import { ConnectionBanner } from "../connection-banner";
import { loginTarget, safeNext } from "../nav";
import { AuthFrame, AuthUnavailable } from "../auth-frame";
import { Shell } from "../shell";
import { NotFoundPage } from "../page";

// The query client is router context so guarded layouts (see admin.tsx) can read the session before rendering.
export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Root,
  notFoundComponent: () => <NotFoundPage title="Page not found" back={{ to: "/", label: "Home" }}>This workspace route does not exist.</NotFoundPage>,
});

// Session refresh before expiry, on focus, and after the tab wakes from sleep
// (a timer that fires late means the machine was suspended).
function useSessionWakeups(queryClient: QueryClient) {
  useEffect(() => {
    const recheck = () => {
      void probeNow();
      void queryClient.invalidateQueries({ queryKey: sessionQueryKey });
    };
    const visible = () => { if (document.visibilityState === "visible") recheck(); };
    let last = Date.now();
    const tick = window.setInterval(() => {
      const now = Date.now();
      if (now - last > 45_000) recheck();
      last = now;
    }, 15_000);
    window.addEventListener("online", recheck);
    window.addEventListener("pageshow", recheck);
    document.addEventListener("visibilitychange", visible);
    return () => {
      window.clearInterval(tick);
      window.removeEventListener("online", recheck);
      window.removeEventListener("pageshow", recheck);
      document.removeEventListener("visibilitychange", visible);
    };
  }, [queryClient]);
}

function Root() {
  return <><ConnectionBanner /><RootContent /></>;
}

function RootContent(): ReactNode {
  const queryClient = useQueryClient();
  const location = useLocation();
  const session = useQuery({
    queryKey: sessionQueryKey,
    queryFn: ({ signal }) => currentSession(signal),
    // A restart or outage never signs anyone out: keep retrying (with the
    // last known session still rendered) until the server answers.
    retry: (_, error) => isUnavailable(error),
    retryDelay: (count) => backoffDelay(count),
    staleTime: 30_000,
    refetchOnWindowFocus: "always",
    refetchIntervalInBackground: true,
    // currentSession renews once the token is within REFRESH_AHEAD_MS of expiry.
    refetchInterval: (query) => {
      const expires = query.state.data?.accessExpiresAt;
      return expires ? Math.max(15_000, Date.parse(expires) - Date.now() - REFRESH_AHEAD_MS + 5_000) : false;
    },
  });
  // Only a first check that failed outright (not an outage) blocks the app.
  const failed = session.isError && session.data === undefined;
  const scope = session.data ? `${session.data.organizationId}:${session.data.principalId}` : null;
  const previousScope = useRef<string | null>(null);
  useSessionWakeups(queryClient);

  useEffect(() => onOtherTabSessionChange(() => {
    const cancelled = queryClient.cancelQueries();
    queryClient.setQueryData(sessionQueryKey, null);
    void cancelled.then(async () => {
      await clearWorkspaceCache(queryClient);
      await queryClient.invalidateQueries({ queryKey: sessionQueryKey });
    });
  }), [queryClient]);

  useEffect(() => {
    if (previousScope.current && previousScope.current !== scope && !failed) {
      void clearWorkspaceCache(queryClient);
    }
    if (!failed) previousScope.current = scope;
  }, [queryClient, scope, failed]);

  if (isAccountLinkRoute(location.pathname)) return <Outlet />;
  if (isPublicCatalogRoute(location.pathname) && (session.isPending || failed || !session.data)) return <Shell><Outlet /></Shell>;
  if (session.isPending) return <AuthFrame><div className="auth-heading" role="status"><h1>Checking your session</h1><p>Connecting to the workspace.</p></div></AuthFrame>;
  if (failed) return <AuthUnavailable retry={() => void session.refetch()} />;
  if (location.pathname === "/login") {
    return session.data ? <Navigate to="/" href={safeNext((location.search as { next?: unknown }).next)} replace /> : <Outlet />;
  }
  if (!session.data) return <Navigate {...loginTarget(location.href)} replace />;
  // A one-time legacy-username session can only set its email; the server
  // refuses everything else, so route there instead of the workspace.
  if (session.data.emailRequired) return location.pathname === emailSetupPath ? <Outlet /> : <Navigate to={emailSetupPath} replace />;
  if (location.pathname === emailSetupPath) return <Navigate to="/me/settings/profile" replace />;
  return <Shell session={session.data}><Outlet /></Shell>;
}
