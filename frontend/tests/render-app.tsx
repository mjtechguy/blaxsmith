// Test harness: renders the real route tree at a path for a given session,
// server-side, so guard and navigation tests see what the browser would.
// gcTime: Infinity leaves no cache timers holding the test process open.
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRouter, RouterProvider } from "@tanstack/react-router";
import { renderToString } from "react-dom/server";
import { sessionQueryKey } from "../src/auth";
import { routeTree } from "../src/routeTree.gen";

export async function renderApp(path: string, session: Record<string, unknown>) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
  queryClient.setQueryData(sessionQueryKey, session);
  const router = createRouter({ routeTree, context: { queryClient }, history: createMemoryHistory({ initialEntries: [path] }) });
  await router.load();
  return renderToString(<QueryClientProvider client={queryClient}><RouterProvider router={router} /></QueryClientProvider>);
}
