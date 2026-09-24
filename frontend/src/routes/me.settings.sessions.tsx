import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { LogOut, Monitor, RefreshCw } from "lucide-react";
import { accountError, browserLabel, listMySessions, mySessionsKey, revokeMyOtherSessions, revokeMySession, useAccount } from "../account";
import type { MySession } from "../gen/blaxsmith/api/v1/account_pb";
import { Card, EmptyState, StatePanel, Timestamp } from "../ui";

export const Route = createFileRoute("/me/settings/sessions")({ component: Sessions });

function Sessions() {
  const { scope } = useAccount();
  const sessions = useQuery({ queryKey: mySessionsKey(scope), enabled: Boolean(scope), queryFn: ({ signal }) => listMySessions(signal) });
  if (sessions.isPending) return <StatePanel kind="loading" title="Loading your sessions" />;
  if (sessions.isError) return <StatePanel kind="error" title="Your sessions are unavailable" retry={() => void sessions.refetch()} />;
  return <SessionList sessions={sessions.data} scope={scope} />;
}

function SessionList({ sessions, scope }: { sessions: MySession[]; scope: string }) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState("");
  const [message, setMessage] = useState<{ error: boolean; text: string } | null>(null);
  const others = sessions.filter((s) => !s.current);
  const run = async (id: string, action: () => Promise<string>) => {
    setBusy(id);
    setMessage(null);
    try {
      setMessage({ error: false, text: await action() });
    } catch (cause) {
      setMessage({ error: true, text: accountError(cause, "The session could not be signed out. Please try again.") });
    } finally {
      setBusy("");
      await queryClient.invalidateQueries({ queryKey: mySessionsKey(scope) });
    }
  };
  const row = (s: MySession) => <li key={s.id} className="session-item">
    <span className="session-icon" aria-hidden="true"><Monitor size={16} /></span>
    <span className="session-main">
      <strong>{browserLabel(s.userAgent)}{s.current ? <span className="state-badge">This session</span> : null}</strong>
      <small>{[s.sourceAddress, s.organizationSlug].filter(Boolean).join(" · ")}</small>
      <small>Signed in <Timestamp value={s.createdAt} />{s.lastSeenAt ? <> · active <Timestamp value={s.lastSeenAt} /></> : null} · expires <time dateTime={s.expiresAt}>{new Date(s.expiresAt).toLocaleDateString(undefined, { dateStyle: "medium" })}</time></small>
    </span>
    {s.current ? null : <button type="button" className="secondary-button" disabled={Boolean(busy)}
      onClick={() => void run(s.id, async () => { await revokeMySession(s.id); return "That session was signed out."; })}>
      {busy === s.id ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <LogOut size={15} aria-hidden="true" />} Sign out</button>}
  </li>;

  return <Card title="Sessions" description="Browsers signed in to your account, in every organization you belong to. Signing one out takes effect within minutes."
    actions={others.length ? <button type="button" className="secondary-button" disabled={Boolean(busy)}
      onClick={() => void run("others", async () => { const r = await revokeMyOtherSessions(); return `${r.revoked} other ${r.revoked === 1n ? "session was" : "sessions were"} signed out.`; })}>
      {busy === "others" ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <LogOut size={15} aria-hidden="true" />} Sign out all others</button> : null}>
    <div className="card-body session-body">
      {message ? <p className={message.error ? "auth-alert" : "notice"} role={message.error ? "alert" : "status"}>{message.text}</p> : null}
      <ul className="session-list" aria-label="Your sessions">{sessions.filter((s) => s.current).map(row)}{others.map(row)}</ul>
      {others.length ? null : <EmptyState title="No other sessions">Only this browser is signed in. Use Sign out in the account menu to end it.</EmptyState>}
    </div>
  </Card>;
}
