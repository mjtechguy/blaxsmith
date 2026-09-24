import { createFileRoute, redirect } from "@tanstack/react-router";

// Source moved into project Settings; the old URL still works.
export const Route = createFileRoute("/projects/$projectId/source")({
  beforeLoad: ({ params }) => { throw redirect({ to: "/projects/$projectId/settings/source", params: { projectId: params.projectId }, replace: true }); },
});
