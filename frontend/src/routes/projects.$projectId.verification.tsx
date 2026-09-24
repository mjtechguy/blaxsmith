import { createFileRoute, redirect } from "@tanstack/react-router";

// Verification moved into project Settings; the old URL still works.
export const Route = createFileRoute("/projects/$projectId/verification")({
  beforeLoad: ({ params }) => { throw redirect({ to: "/projects/$projectId/settings/verification", params: { projectId: params.projectId }, replace: true }); },
});
