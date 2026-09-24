import { createFileRoute, redirect } from "@tanstack/react-router";

// Moved to Library › Recipes; the old URL (and its ?from=) still works.
export const Route = createFileRoute("/admin/recipes/$recipeId/versions/new")({
  validateSearch: (search: Record<string, unknown>): { from?: string } => (typeof search.from === "string" ? { from: search.from } : {}),
  beforeLoad: ({ params, search }) => {
    throw redirect({ to: "/recipes/$recipeId/versions/new", params: { recipeId: params.recipeId }, search, replace: true });
  },
});
