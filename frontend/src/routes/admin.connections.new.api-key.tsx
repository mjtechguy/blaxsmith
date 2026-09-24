import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { NewApiKey } from "../connection-ui";

export const Route = createFileRoute("/admin/connections/new/api-key")({ component: AdminNewApiKey });

function AdminNewApiKey() {
  const navigate = useNavigate();
  return <NewApiKey scope="organization" cancel={<Link to="/admin/connections" className="secondary-button">Cancel</Link>}
    flow={{ title: "Add an organization API key", description: "Organization-owned; grant it to projects, users, or roles afterwards.", back: { href: "/admin/connections", label: "Connections" }, typeHref: "/admin/connections/new" }}
    onDone={(c) => void navigate({ to: "/admin/connections/$connectionId", params: { connectionId: c.id }, search: { tab: "grants" } as never })} />;
}
