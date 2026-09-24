import { createFileRoute, redirect } from "@tanstack/react-router";

// Organization recipes are managed in Library › Recipes now; the old URL still works.
export const Route = createFileRoute("/admin/recipes/")({
  beforeLoad: () => { throw redirect({ to: "/recipes", replace: true }); },
});
