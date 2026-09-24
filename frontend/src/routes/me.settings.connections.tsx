import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight } from "lucide-react";
import { Card } from "../ui";

export const Route = createFileRoute("/me/settings/connections")({ component: Connections });

function Connections() {
  return <Card title="Connections" description="Your own subscriptions and API keys serve only runs you launch.">
    <ul className="link-list"><li><Link to="/me/connections">Open My connections <ArrowRight size={13} aria-hidden="true" /></Link></li></ul>
  </Card>;
}
