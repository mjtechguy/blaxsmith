import { createFileRoute } from "@tanstack/react-router";
import { accountActions, useAccount } from "../account";
import { SummaryList } from "../layouts";
import { Card, Timestamp } from "../ui";

export const Route = createFileRoute("/me/settings/sessions")({ component: Sessions });

function Sessions() {
  const { account } = useAccount();
  return <>
    <Card title="This session" description="The browser session you are using now. It refreshes itself while you work.">
      <SummaryList items={[{ label: "Access renews", value: account?.accessExpiresAt ? <Timestamp value={account.accessExpiresAt} /> : "—" }]} />
    </Card>
    {!accountActions ? <section className="coming-soon" aria-labelledby="sessions-soon">
      <span className="soon-badge">Coming soon</span>
      <h2 id="sessions-soon">Other sessions</h2>
      <p>A list of your other signed-in browsers, with sign-out per session, is not available yet. Signing out from the account menu ends this session; an organization admin can sign you out everywhere from Admin › Users.</p>
    </section> : null}
  </>;
}
