import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowLeft, ArrowRight, Ban, ExternalLink, KeyRound, LogIn, Plus, RefreshCw, UserRound } from "lucide-react";
import { failure, useOrg } from "../connection-ui";
import { CLAUDE_SUBSCRIPTION_REASON, createCodexSubscription, pollCodexDeviceLogin, startCodexDeviceLogin } from "../connections";
import { TextField } from "../form-field";
import type { StartCodexDeviceLoginResponse } from "../gen/blaxsmith/api/v1/connections_pb";
import { PageHeader, PageShell } from "../page";

export const Route = createFileRoute("/me/connections/new/subscription")({ component: NewSubscription });

function NewSubscription() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [login, setLogin] = useState<StartCodexDeviceLoginResponse | null>(null);
  const [error, setError] = useState("");
  const [authJson, setAuthJson] = useState("");
  const done = async (connectionId: string) => {
    await queryClient.invalidateQueries({ queryKey: ["connections", org] });
    await navigate({ to: "/me/connections/$connectionId", params: { connectionId } });
  };
  const start = useMutation({
    mutationFn: startCodexDeviceLogin,
    onSuccess: (r) => { setError(""); setLogin(r); },
    onError: (cause) => setError(failure(cause, "ChatGPT sign-in could not start. Please try again.")),
  });
  const poll = useQuery({
    queryKey: ["codex-device-login", org, login?.loginId ?? ""], enabled: Boolean(login),
    queryFn: ({ signal }) => pollCodexDeviceLogin(login!.loginId, signal),
    refetchInterval: (query) => query.state.data && query.state.data.state !== "pending" ? false : Math.max(login?.intervalSeconds ?? 5, 2) * 1000,
    retry: false, gcTime: 0,
  });
  const state = poll.data?.state ?? "pending";
  const connectedId = poll.data?.state === "connected" ? poll.data.connection?.id ?? "" : "";
  useEffect(() => { if (connectedId) void done(connectedId); }, [connectedId]);
  const paste = useMutation({
    mutationFn: (value: string) => createCodexSubscription(value),
    onSuccess: async (c) => { if (c) await done(c.id); },
    onError: (cause) => setError(failure(cause, "That is not a ChatGPT sign-in auth.json with a refresh token.")),
  });

  return <PageShell>
    <PageHeader eyebrow="Personal / Connections" title="Connect a subscription" description="Coding-plan logins are always personal: only runs you launch can use them, whichever page you started from." />
    <Link to="/me/connections" className="text-action"><ArrowLeft size={15} aria-hidden="true" /> Back to my connections</Link>
    <div className="editor-layout">
      <section className="editor-card" aria-labelledby="codex-heading">
        <div className="editor-card-heading"><span className="project-symbol"><UserRound size={18} aria-hidden="true" /></span><div><h2 id="codex-heading">Codex (ChatGPT plan)</h2><p>Sign in with your ChatGPT account. The platform keeps the refresh token and gives each run a short-lived access token.</p></div></div>
        <div className="editor-form">
          {!login ? <button type="button" className="primary-button" disabled={start.isPending} onClick={() => start.mutate()}>
            {start.isPending ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <LogIn size={15} aria-hidden="true" />} Sign in with ChatGPT</button> : null}
          {login && state === "pending" ? <div className="device-login" role="status" aria-live="polite">
            <p>1. Open <a className="text-action" href={login.verificationUrl} target="_blank" rel="noreferrer">{login.verificationUrl} <ExternalLink size={13} aria-hidden="true" /></a> and sign in.</p>
            <p>2. Enter this one-time code{login.expiresAt ? ` before ${new Date(login.expiresAt).toLocaleTimeString()}` : ""}:</p>
            <code className="device-code">{login.userCode}</code>
            <p className="admin-note"><RefreshCw size={12} className="spin" aria-hidden="true" /> Waiting for approval… Continue only if you started this sign-in here.</p>
          </div> : null}
          {login && (state === "expired" || state === "failed" || poll.isError) ? <div className="notice" role="alert"><strong>{state === "expired" ? "The code expired." : "Sign-in failed."}</strong> {poll.data?.error}
            <button type="button" className="text-action" onClick={() => { setLogin(null); start.mutate(); }}>Start again</button></div> : null}
          {state === "connected" ? <p className="notice" role="status">Connected. Opening the connection…</p> : null}
          {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        </div>
        <details className="advanced-disclosure"><summary>Advanced: paste auth.json</summary>
          <form className="editor-form" noValidate onSubmit={(event) => {
            event.preventDefault();
            const value = authJson.trim();
            setAuthJson("");
            if (!value.startsWith("{") || new TextEncoder().encode(value).length > 12288) { setError("Paste the full contents of ~/.codex/auth.json."); return; }
            setError(""); paste.mutate(value);
          }}>
            <p className="admin-note">On your own machine run <span className="mono">codex login --device-auth</span>, then paste <span className="mono">~/.codex/auth.json</span>. It is write-only.</p>
            <TextField label="auth.json contents" name="auth-json" type="password" autoComplete="new-password" placeholder="Paste ~/.codex/auth.json" value={authJson} onChange={setAuthJson} onBlur={() => {}} />
            <div className="editor-actions"><button type="submit" className="secondary-button" disabled={!authJson.trim() || paste.isPending}><Plus size={15} aria-hidden="true" /> {paste.isPending ? "Connecting…" : "Connect with auth.json"}</button></div>
          </form>
        </details>
      </section>
      <aside className="editor-note">
        <h2>OpenCode</h2>
        <p>OpenCode Zen and OpenCode Go (its subscription) use API keys; OpenCode offers no OAuth sign-in for its own provider. <Link to="/me/connections/new/api-key" className="text-action"><KeyRound size={13} aria-hidden="true" /> Add an OpenCode key <ArrowRight size={13} aria-hidden="true" /></Link></p>
        <h2>Claude (Pro/Max)</h2>
        <p><button type="button" className="secondary-button" disabled aria-describedby="claude-reason"><Ban size={15} aria-hidden="true" /> Claude subscription unavailable</button></p>
        <p id="claude-reason">{CLAUDE_SUBSCRIPTION_REASON}</p>
      </aside>
    </div>
  </PageShell>;
}
