import { createFileRoute, redirect } from "@tanstack/react-router";

// Moved to Library › Recipes, which carries the same admin actions; the old URL still works.
export const Route = createFileRoute("/admin/recipes/$recipeId/")({
  beforeLoad: ({ params, location }) => {
    throw redirect({ to: "/recipes/$recipeId", params: { recipeId: params.recipeId }, search: (location.search ?? {}) as never, replace: true });
  },
});
