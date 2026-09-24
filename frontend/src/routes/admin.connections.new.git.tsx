import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { NewGit } from "../connection-ui";

export const Route = createFileRoute("/admin/connections/new/git")({ component: AdminNewGit });

function AdminNewGit() {
  const navigate = useNavigate();
  return <NewGit scope="organization" returnTo="/admin/connections" cancel={<Link to="/admin/connections" className="secondary-button">Cancel</Link>}
    flow={{ title: "Connect Git", description: "An organization GitHub or GitLab account for private sources and run-branch pushes.", back: { href: "/admin/connections", label: "Connections" }, typeHref: "/admin/connections/new" }}
    onDone={(c) => void navigate({ to: "/admin/connections/$connectionId", params: { connectionId: c.id } })} />;
}
