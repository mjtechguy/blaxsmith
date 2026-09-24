import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, GitBranch, KeyRound, UserRound } from "lucide-react";
import { CreateFlow } from "../layouts";

export const Route = createFileRoute("/admin/connections/new/")({ component: ChooseType });

const types = [
  { href: "/admin/connections/new/api-key", icon: KeyRound, title: "Provider API key", body: "Anthropic, OpenAI, or OpenCode. Checked with the provider; its models become available to grant." },
  { href: "/admin/connections/new/git", icon: GitBranch, title: "Git access", body: "Sign in with GitHub or add a token for private sources and run-branch pushes." },
  { href: "/me/connections/new/subscription", icon: UserRound, title: "Subscription (personal)", body: "Subscriptions belong to one person and serve only their own runs, so they are added under My connections." },
];

function ChooseType() {
  return <CreateFlow title="Add a connection" description="Choose what to connect. Credentials are write-only and never shown again."
    back={{ href: "/admin/connections", label: "Connections" }}
    steps={[{ id: "type", label: "Type", state: "current" }, { id: "details", label: "Details", state: "todo" }, { id: "validate", label: "Validate", state: "todo" }, { id: "grants", label: "Grants", state: "todo" }]}
    summary={<><h2>What happens next</h2><p className="form-hint">After the provider accepts the credential you grant the connection to projects, users, or a minimum role. Owners and admins get no implicit use.</p></>}>
    <ul className="choice-list">{types.map((t) => <li key={t.href}><Link to={t.href as "/"} className="choice-card">
      <span className="project-symbol"><t.icon size={17} aria-hidden="true" /></span>
      <span><strong>{t.title}</strong><small>{t.body}</small></span><ArrowRight size={15} aria-hidden="true" /></Link></li>)}</ul>
  </CreateFlow>;
}
