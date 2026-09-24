// Test harness: renders the real route tree at a path for a given session,
// server-side, so guard and navigation tests see what the browser would.
// gcTime: Infinity leaves no cache timers holding the test process open.
import type { ReactNode } from "react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory, createRootRoute, createRouter, RouterProvider } from "@tanstack/react-router";
import { renderToString } from "react-dom/server";
import { sessionQueryKey } from "../src/auth";
import { routeTree } from "../src/routeTree.gen";

function setup(path: string, session: Record<string, unknown>) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
  queryClient.setQueryData(sessionQueryKey, session);
  const router = createRouter({ routeTree, context: { queryClient }, history: createMemoryHistory({ initialEntries: [path] }) });
  return { queryClient, router };
}

export async function renderApp(path: string, session: Record<string, unknown>) {
  const { queryClient, router } = setup(path, session);
  await router.load();
  return renderToString(<QueryClientProvider client={queryClient}><RouterProvider router={router} /></QueryClientProvider>);
}

// A DetailLayout with tabs, facts, a disclosure, and a copyable ID, as the run page uses it.
export async function renderDetailFixture() {
  const { DetailLayout } = await import("../src/layouts");
  const { CopyValue, Disclosure } = await import("../src/ui");
  return renderInRouter(<DetailLayout title="guild-demo" status={<span>active</span>} facts={[{ label: "Commit", value: "abc" }]} current="stages" tabsLabel="Run sections"
    tabs={[{ id: "overview", label: "Overview" }, { id: "stages", label: "Stages", count: 6 }, { id: "review", label: "Review", hidden: true }]}>
    <Disclosure summary="Advanced">hidden body</Disclosure><CopyValue value="0123456789abcdef" label="Run ID" />
  </DetailLayout>, "/projects/p/runs/r?tab=stages");
}

// Renders one component inside a router (so Link works) at a path.
export async function renderInRouter(node: ReactNode, path = "/") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity, gcTime: Infinity } } });
  const rootRoute = createRootRoute({ component: () => <>{node}</> });
  const router = createRouter({ routeTree: rootRoute, history: createMemoryHistory({ initialEntries: [path] }) });
  await router.load();
  return renderToString(<QueryClientProvider client={queryClient}><RouterProvider router={router as never} /></QueryClientProvider>);
}

// A setup checklist with the given steps (done: true, false, or undefined while loading).
export async function renderChecklist(items: Array<{ id: string; done: boolean | undefined }>) {
  const { SetupChecklist } = await import("../src/setup-checklist");
  return renderInRouter(<SetupChecklist title="Set up this project" storageKey="test-checklist"
    items={items.map((i) => ({ ...i, label: `Step ${i.id}`, hint: `Do ${i.id}`, to: "/" }))} />);
}
