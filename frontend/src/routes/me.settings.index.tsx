import { createFileRoute, redirect } from "@tanstack/react-router";

export const Route = createFileRoute("/me/settings/")({
  beforeLoad: () => { throw redirect({ to: "/me/settings/profile", replace: true }); },
});
