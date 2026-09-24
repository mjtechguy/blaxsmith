import { createFileRoute, redirect } from "@tanstack/react-router";

// Model access folded into the project connections hub; the old URLs still work.
// The subscription child redirects to the personal hub itself.
export const Route = createFileRoute("/projects/$projectId/model-access")({
  beforeLoad: ({ params, location }) => {
    if (location.pathname.endsWith("/model-access/subscription")) return;
    throw redirect({ to: "/projects/$projectId/connections", params: { projectId: params.projectId }, replace: true });
  },
});
