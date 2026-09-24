import { createFileRoute, redirect } from "@tanstack/react-router";

// Subscriptions are personal and live under My connections now; the old URL still works.
export const Route = createFileRoute("/projects/$projectId/model-access/subscription")({
  beforeLoad: () => { throw redirect({ to: "/me/connections/new/subscription", replace: true }); },
});
