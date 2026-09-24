import { useEffect, useRef } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createRootRoute, Navigate, Outlet, useLocation } from "@tanstack/react-router";
import { clearWorkspaceCache, currentSession, isAccountLinkRoute, isPublicCatalogRoute, onOtherTabSessionChange, sessionQueryKey } from "../auth";
import { AuthFrame, AuthUnavailable } from "../auth-frame";
import { Shell } from "../shell";

export const Route = createRootRoute({
  component: Root,
  notFoundComponent: () => <div className="state-panel"><h1>Page not found</h1><p>This workspace route does not exist.</p></div>,
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
  if (session.isPending) return <AuthFrame><div className="auth-heading" role="status"><h2>Checking your session</h2><p>Connecting to the workspace.</p></div></AuthFrame>;
  if (unavailable) return <AuthUnavailable retry={() => void session.refetch()} />;
  if (location.pathname === "/login") {
    const next = location.search.next === "/tools" ? "/tools" : "/";
    return session.data ? <Navigate to={next} replace /> : <Outlet />;
  }
  if (!session.data) return <Navigate to="/login" search={{ next: location.pathname === "/tools" ? "/tools" : "/" }} replace />;
  return <Shell session={session.data}><Outlet /></Shell>;
}
