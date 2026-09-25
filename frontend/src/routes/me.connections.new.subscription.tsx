import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { ArrowRight, Ban, KeyRound, LogIn, Plus, UserRound } from "lucide-react";
import { connectionSteps, failure, scopeNote, useOrg } from "../connection-ui";
import { CLAUDE_SUBSCRIPTION_REASON, claudePolicyKey, claudeSetupTokenShape, createClaudeSubscription, createCodexSubscription, getClaudeSubscriptionPolicy, pollCodexDeviceLogin, startCodexDeviceLogin } from "../connections";
import { TextField } from "../form-field";
import { CreateFlow } from "../layouts";
import { SignInStatus, useSignIn } from "../sign-in-flow";
import { Disclosure } from "../ui";

export const Route = createFileRoute("/me/connections/new/subscription")({ component: NewSubscription });

function NewSubscription() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [signIn, dispatch] = useSignIn();
  const [loginId, setLoginId] = useState("");
  const [interval, setIntervalSeconds] = useState(5);
  const [error, setError] = useState("");
  const [authJson, setAuthJson] = useState("");
  const done = async (connectionId: string) => {
    await queryClient.invalidateQueries({ queryKey: ["connections", org] });
    await navigate({ to: "/me/connections/$connectionId", params: { connectionId } });
  };
  const start = useMutation({
    mutationFn: startCodexDeviceLogin,
    onMutate: () => dispatch({ type: "start" }),
    onSuccess: (r) => {
      setError(""); setLoginId(r.loginId); setIntervalSeconds(r.intervalSeconds);
      dispatch({ type: "started", device: { verificationUrl: r.verificationUrl, userCode: r.userCode, expiresAt: r.expiresAt } });
    },
    onError: (cause) => dispatch({ type: "fail", error: failure(cause, "ChatGPT sign-in could not start. Please try again.") }),
  });
  // Polls only while waiting: cancelling or expiring stops it, and a late
  // answer cannot move a cancelled flow (see sign-in.ts).
  const poll = useQuery({
    queryKey: ["codex-device-login", org, loginId, signIn.attempt], enabled: Boolean(loginId) && signIn.phase === "waiting",
    queryFn: ({ signal }) => pollCodexDeviceLogin(loginId, signal),
    refetchInterval: (query) => query.state.data && query.state.data.state !== "pending" ? false : Math.max(interval, 2) * 1000,
    retry: false, gcTime: 0,
  });
  const result = poll.data?.state;
  const connectedId = result === "connected" ? poll.data?.connection?.id ?? "" : "";
  useEffect(() => {
    if (result === "connected") { dispatch({ type: "verify" }); dispatch({ type: "succeed" }); if (connectedId) void done(connectedId); }
    if (result === "expired") dispatch({ type: "expire" });
    if (result === "failed") dispatch({ type: "fail", error: poll.data?.error || "ChatGPT sign-in failed." });
  }, [result, connectedId]);
  useEffect(() => { if (poll.isError) dispatch({ type: "fail", error: "The sign-in status could not be checked." }); }, [poll.isError]);
  const paste = useMutation({
    mutationFn: (value: string) => createCodexSubscription(value),
    onSuccess: async (c) => { if (c) await done(c.id); },
    onError: (cause) => setError(failure(cause, "That is not a ChatGPT sign-in auth.json with a refresh token.")),
  });

  // Sign in → Validate (the device code and its poll) → Use in a project.
  return <CreateFlow title="Connect a subscription" description="Coding-plan logins are always personal: only runs you launch can use them, whichever page you started from."
    back={{ href: "/me/connections", label: "My connections" }} steps={connectionSteps({}, "personal", signIn, "Sign in")}
    summary={<>
      <h2>Summary</h2>
      <p className="form-hint">{scopeNote("personal")}</p>
      <h2>OpenCode</h2>
      <p className="form-hint">OpenCode Zen and OpenCode Go (its subscription) use API keys; OpenCode offers no OAuth sign-in for its own provider. <Link to="/me/connections/new/api-key" className="text-action"><KeyRound size={13} aria-hidden="true" /> Add an OpenCode key <ArrowRight size={13} aria-hidden="true" /></Link></p>
    </>}>
      <section className="editor-card" aria-labelledby="codex-heading">
        <div className="editor-card-heading"><span className="project-symbol"><UserRound size={18} aria-hidden="true" /></span><div><h2 id="codex-heading">Codex (ChatGPT plan)</h2><p>Sign in with your ChatGPT account. The platform keeps the refresh token and gives each run a short-lived access token.</p></div></div>
        <div className="editor-form">
          {signIn.phase === "idle" ? <button type="button" className="primary-button" onClick={() => start.mutate()}>
            <LogIn size={15} aria-hidden="true" /> Sign in with ChatGPT</button> : null}
          <SignInStatus state={signIn} onCancel={() => dispatch({ type: "cancel" })} onExpire={() => dispatch({ type: "expire" })}
            onRetry={() => { dispatch({ type: "retry" }); setLoginId(""); }}
            labels={{ starting: "Requesting a sign-in code…", waiting: "Waiting for approval… Continue only if you started this sign-in here.",
              verifying: "Storing the sign-in…", succeeded: "Connected. Opening the connection…" }} />
          {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        </div>
        <Disclosure summary="Advanced: paste auth.json">
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
        </Disclosure>
      </section>
      <ClaudeSubscription onConnected={done} />
  </CreateFlow>;
}

// A member's own `claude setup-token` (docs/model-gateway-plan.md §6.1): only
// runs this member starts can use it, and only while the org allows it.
function ClaudeSubscription({ onConnected }: { onConnected: (connectionId: string) => Promise<void> }) {
  const { org } = useOrg();
  const [token, setToken] = useState("");
  const [error, setError] = useState("");
  const policy = useQuery({ queryKey: claudePolicyKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getClaudeSubscriptionPolicy(signal) });
  const connect = useMutation({
    mutationFn: createClaudeSubscription,
    onSuccess: async (c) => { if (c) await onConnected(c.id); },
    onError: (cause) => setError(failure(cause, "That is not a claude setup-token, or your organization has turned this off.")),
  });
  const allowed = policy.data === true;
  return <section className="editor-card" aria-labelledby="claude-heading">
    <div className="editor-card-heading"><span className="project-symbol"><UserRound size={18} aria-hidden="true" /></span><div><h2 id="claude-heading">Claude (Pro/Max)</h2>
      <p>Use your own Claude plan for runs you start. The token is stored encrypted, never shown again, never shared or pooled, and a run using it can be taken over only by you.</p></div></div>
    {!allowed ? <div className="editor-form">
      <p><button type="button" className="secondary-button" disabled aria-describedby="claude-reason"><Ban size={15} aria-hidden="true" /> {policy.isPending ? "Checking…" : "Claude subscription unavailable"}</button></p>
      {policy.isPending ? null : <p id="claude-reason" className="form-hint">{CLAUDE_SUBSCRIPTION_REASON}</p>}
    </div> : <form className="editor-form" noValidate onSubmit={(event) => {
      event.preventDefault();
      const value = token.trim();
      setToken("");
      if (!claudeSetupTokenShape.test(value)) { setError("Paste the token that claude setup-token printed. It starts with sk-ant-oat."); return; }
      setError(""); connect.mutate(value);
    }}>
      <p className="admin-note">On your own machine run <span className="mono">claude setup-token</span>, sign in, then paste the token it prints. It is write-only.</p>
      <TextField label="Setup token" name="claude-setup-token" type="password" autoComplete="new-password" placeholder="sk-ant-oat…" value={token} onChange={setToken} onBlur={() => {}} />
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <div className="editor-actions"><button type="submit" className="primary-button" disabled={!token.trim() || connect.isPending}><Plus size={15} aria-hidden="true" /> {connect.isPending ? "Connecting…" : "Connect Claude subscription"}</button></div>
    </form>}
  </section>;
}
