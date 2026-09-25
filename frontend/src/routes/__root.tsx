import { useEffect, useRef } from "react";
import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { createRootRouteWithContext, Navigate, Outlet, useLocation } from "@tanstack/react-router";
import { clearWorkspaceCache, currentSession, emailSetupPath, isAccountLinkRoute, isPublicCatalogRoute, onOtherTabSessionChange, sessionQueryKey } from "../auth";
import { safeNext } from "../nav";
import { AuthFrame, AuthUnavailable } from "../auth-frame";
import { Shell } from "../shell";
import { NotFoundPage } from "../page";

// The query client is router context so guarded layouts (see admin.tsx) can read the session before rendering.
export const Route = createRootRouteWithContext<{ queryClient: QueryClient }>()({
  component: Root,
  notFoundComponent: () => <NotFoundPage title="Page not found" back={{ to: "/", label: "Home" }}>This workspace route does not exist.</NotFoundPage>,
});

function Root() {
  const queryClient = useQueryClient();
  const location = useLocation();
  const session = useQuery({
    queryKey: sessionQueryKey,
    queryFn: ({ signal }) => currentSession(signal),
    retry: false,
    staleTime: 30_000,
    refetchOnWindowFocus: "always",
    refetchInterval: (query) => {
      const expires = query.state.data?.accessExpiresAt;
      return expires ? Math.max(15_000, Date.parse(expires) - Date.now() - 60_000) : false;
    },
  });
  const unavailable = session.isError || session.isRefetchError;
  const scope = unavailable ? null : session.data ? `${session.data.organizationId}:${session.data.principalId}` : null;
  const previousScope = useRef<string | null>(null);

  useEffect(() => onOtherTabSessionChange(() => {
    const cancelled = queryClient.cancelQueries();
    queryClient.setQueryData(sessionQueryKey, null);
    void cancelled.then(async () => {
      await clearWorkspaceCache(queryClient);
      await queryClient.invalidateQueries({ queryKey: sessionQueryKey });
    });
  }), [queryClient]);

  useEffect(() => {
    if (previousScope.current && previousScope.current !== scope) {
      void clearWorkspaceCache(queryClient);
    }
    previousScope.current = scope;
  }, [queryClient, scope]);

  if (isAccountLinkRoute(location.pathname)) return <Outlet />;
  if (isPublicCatalogRoute(location.pathname) && (session.isPending || unavailable || !session.data)) return <Shell><Outlet /></Shell>;
  if (session.isPending) return <AuthFrame><div className="auth-heading" role="status"><h1>Checking your session</h1><p>Connecting to the workspace.</p></div></AuthFrame>;
  if (unavailable) return <AuthUnavailable retry={() => void session.refetch()} />;
  if (location.pathname === "/login") {
    return session.data ? <Navigate to="/" href={safeNext((location.search as { next?: unknown }).next)} replace /> : <Outlet />;
  }
  if (!session.data) return <Navigate to="/login" search={{ next: safeNext(location.href) }} replace />;
  // A one-time legacy-username session can only set its email; the server
  // refuses everything else, so route there instead of the workspace.
  if (session.data.emailRequired) return location.pathname === emailSetupPath ? <Outlet /> : <Navigate to={emailSetupPath} replace />;
  if (location.pathname === emailSetupPath) return <Navigate to="/me/settings/profile" replace />;
  return <Shell session={session.data}><Outlet /></Shell>;
}
