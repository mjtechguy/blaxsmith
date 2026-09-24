import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { NewApiKey } from "../connection-ui";

export const Route = createFileRoute("/me/connections/new/api-key")({ component: MyNewApiKey });

function MyNewApiKey() {
  const navigate = useNavigate();
  return <NewApiKey scope="personal" cancel={<Link to="/me/connections" className="secondary-button">Cancel</Link>}
    flow={{ title: "Add a personal API key", description: "Yours only; it serves runs you launch.", back: { href: "/me/connections", label: "My connections" } }}
    onDone={(c) => void navigate({ to: "/me/connections/$connectionId", params: { connectionId: c.id }, search: { tab: "usage" } as never })} />;
}
