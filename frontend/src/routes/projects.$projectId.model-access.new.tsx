import { createFileRoute, redirect } from "@tanstack/react-router";

// Model access folded into the project connections hub; the old URL still works.
export const Route = createFileRoute("/projects/$projectId/model-access/new")({
  beforeLoad: ({ params }) => { throw redirect({ to: "/projects/$projectId/connections", params: { projectId: params.projectId }, replace: true }); },
});
