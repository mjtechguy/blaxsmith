import { createRootRoute, Outlet } from "@tanstack/react-router";
import { Shell } from "../shell";

export const Route = createRootRoute({
  component: () => <Shell><Outlet /></Shell>,
  notFoundComponent: () => <div className="state-panel"><h1>Page not found</h1><p>This workspace route does not exist.</p></div>,
});
