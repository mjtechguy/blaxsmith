import { createFileRoute, redirect } from "@tanstack/react-router";

// Moved to Library › Recipes; the old URL (and its ?from=) still works.
export const Route = createFileRoute("/admin/recipes/new")({
  validateSearch: (search: Record<string, unknown>): { from?: string } => (typeof search.from === "string" ? { from: search.from } : {}),
  beforeLoad: ({ search }) => { throw redirect({ to: "/recipes/new", search, replace: true }); },
});
