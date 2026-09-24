import { createFileRoute } from "@tanstack/react-router";
import { accountActions, useAccount } from "../account";
import { SummaryList } from "../layouts";
import { Card, CopyValue } from "../ui";

export const Route = createFileRoute("/me/settings/profile")({ component: Profile });

const roleLabel: Record<string, string> = { owner: "Owner", admin: "Admin", member: "Member", viewer: "Viewer" };

function Profile() {
  const { account, loading } = useAccount();
  const pending = <span className="muted">{loading ? "Loading…" : "—"}</span>;
  return <>
    <Card title="Profile" description="How you appear to your organization.">
      <SummaryList items={[
        { label: "Display name", value: account?.displayName || (loading ? pending : <span className="muted">Not set</span>) },
        { label: "Username", value: account?.username || pending },
        { label: "Organization", value: account?.organizationName || pending },
        { label: "Role", value: account ? roleLabel[account.role] ?? account.role : pending },
        { label: "Account ID", value: account ? <CopyValue value={account.principalId} label="Account ID" chars={13} /> : pending },
      ]} />
    </Card>
    {!accountActions ? <section className="coming-soon" aria-labelledby="profile-soon">
      <span className="soon-badge">Coming soon</span>
      <h2 id="profile-soon">Editing your profile</h2>
      <p>Changing your display name or email is not available yet. Ask an organization admin if something here is wrong. Your role is set by an owner or admin.</p>
    </section> : null}
  </>;
}
