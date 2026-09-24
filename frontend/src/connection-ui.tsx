import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { Link, useNavigate } from "@tanstack/react-router";
import { ArrowRight, GitBranch, KeyRound, Pin, Plus, RefreshCw, Trash2 } from "lucide-react";
import { ago } from "./admin";
import { currentSession, sessionQueryKey } from "./auth";
import {
  addConnectionUse, apiKeyProviders, authLabel, connectionModelsKey, connectionsKey, createApiKeyConnection, createGitTokenConnection,
  getGitHubApp, gitHubAppKey, grantConnection, healthFix, kindLabel, listConnectionModels, listConnections, modelsSummary, providerLabel,
  refreshConnectionModels, removeConnectionUse, revokeConnection, setRecommendedModels, revokeConnectionGrant, scopeLabel, startGitHubConnect,
  type ListScope, type Scope,
} from "./connections";
import { DataTable } from "./data-table";
import { TextField } from "./form-field";
import { AccessCheck, AccessExplanation } from "./access-explain";
import { ModelSelect } from "./model-select";

export { ModelSelect };
import { isBusy, type SignInState } from "./sign-in";
import { SignInStatus, useSignIn } from "./sign-in-flow";
import type { Connection, ConnectionGrant, ConnectionModel, ConnectionUse } from "./gen/blaxsmith/api/v1/connections_pb";
import { listProjects } from "./workflow";

export function useOrg() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  return { session, org: session.data?.organizationId || "" };
}

export function failure(cause: unknown, fallback: string): string {
  const error = ConnectError.from(cause);
  if (error.code === Code.PermissionDenied) return "Your session is not allowed to do this.";
  if (error.code === Code.Unauthenticated) return "Your session changed. Sign in again and retry.";
  if ((error.code === Code.InvalidArgument || error.code === Code.FailedPrecondition || error.code === Code.AlreadyExists) && error.rawMessage) return error.rawMessage;
  return fallback;
}

export function StateBadge({ state }: { state: string }) {
  const tone = state === "active" ? "state-succeeded" : state === "reconnect_required" ? "state-waiting" : "state-failed";
  return <span className={`state-badge ${tone}`}>{state.replaceAll("_", " ")}</span>;
}

// A non-secret identity (account id, username, key label) stays blurred
// until clicked, so a shared screen does not show it by default.
export function RedactedText({ text, label }: { text: string; label: string }) {
  const [shown, setShown] = useState(false);
  if (!text) return null;
  return <button type="button" className={shown ? "redacted is-shown" : "redacted"} aria-pressed={shown}
    aria-label={shown ? `${label}: ${text}. Hide` : `Reveal ${label}`} onClick={() => setShown(!shown)}>
    <span aria-hidden="true">{text}</span></button>;
}

const healthTone: Record<string, string> = { ready: "state-succeeded", warning: "state-waiting", error: "state-failed", disabled: "state-blocked" };

// State, sign-in and identity, last check, and the reason with its fix.
export function HealthLine({ connection, compact = false }: { connection: Connection; compact?: boolean }) {
  const h = connection.health;
  if (!h) return <StateBadge state={connection.state} />;
  const fix = healthFix(connection);
  return <span className={compact ? "health-line health-compact" : "health-line"}>
    <span className={`state-badge ${healthTone[h.state] ?? ""}`}>{h.state}</span>
    {!compact ? <span>{authLabel(h.auth)}{h.identity ? <> · <RedactedText text={h.identity} label="identity" /></> : null}</span> : null}
    {!compact ? <span>{h.checkedAt ? <>Checked <time dateTime={h.checkedAt}>{ago(h.checkedAt)}</time></> : "Never checked"}</span> : null}
    {h.message ? <span className={h.state === "error" ? "form-field-error" : undefined}>{h.message}</span> : null}
    {fix ? <Link className="text-action" to={fix.to as "/"}>{fix.label} <ArrowRight size={13} aria-hidden="true" /></Link> : null}
  </span>;
}

export function Loading({ label }: { label: string }) {
  return <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>{label}</h2></div>;
}

export function LoadError({ label, retry }: { label: string; retry: () => void }) {
  return <div className="state-panel" role="alert"><h2>{label}</h2><p>This could not be loaded.</p><button type="button" className="secondary-button" onClick={retry}>Try again</button></div>;
}

// App confirmation for destructive decisions; creation stays on routed pages.
export function ConfirmDialog({ title, body, confirmLabel, busy, error, onConfirm, onClose }: {
  title: string; body: ReactNode; confirmLabel: string; busy: boolean; error: string; onConfirm: () => void; onClose: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => { dialog.current?.showModal(); }, []);
  return <dialog ref={dialog} className="review-confirm" aria-labelledby="connection-confirm-title"
    onCancel={(event) => { if (busy) event.preventDefault(); }} onClose={() => { if (!busy) onClose(); }}>
    <h3 id="connection-confirm-title">{title}</h3>
    <p>{body}</p>
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    <div className="review-confirm-actions">
      <button type="button" className="secondary-button" disabled={busy} onClick={onClose}>Cancel</button>
      <button type="button" className="primary-button danger-button" disabled={busy} onClick={onConfirm}>{busy ? "Working…" : confirmLabel}</button>
    </div>
  </dialog>;
}

const features = tableFeatures({});

// explainIn adds a "Where from" column: the access chain in that project.
export function ConnectionTable({ connections, label, empty, manage, explainIn }: {
  connections: Connection[]; label: string; empty: string; manage: (c: Connection) => ReactNode; explainIn?: string;
}) {
  const columns = useMemo<ColumnDef<typeof features, Connection>[]>(() => [
    { id: "provider", header: "Provider", cell: ({ row }) => <span className="task-stage"><strong>{providerLabel(row.original.provider)}</strong><small>{kindLabel(row.original.kind)}{row.original.label ? ` · ${row.original.label}` : ""}</small></span> },
    { id: "scope", header: "Scope / owner", cell: ({ row }) => <span className="task-stage"><strong>{scopeLabel(row.original.scope)}</strong><small>{row.original.ownerName || row.original.ownerId.slice(0, 8)}</small></span> },
    { id: "account", header: "Account", cell: ({ row }) => row.original.account ? <span className="mono admin-wrap"><RedactedText text={row.original.account} label="account" /></span> : "—" },
    { id: "state", header: "Health", cell: ({ row }) => <HealthLine connection={row.original} compact /> },
    { id: "models", header: "Models", cell: ({ row }) => <span className={row.original.modelsError ? "form-field-error" : undefined}>{modelsSummary(row.original)}</span> },
    { id: "grants", header: "Grants / uses", cell: ({ row }) => `${row.original.grants.length} / ${row.original.uses.length}` },
    { id: "used", header: "Last used", cell: ({ row }) => row.original.lastUsedAt ? <time dateTime={row.original.lastUsedAt}>{ago(row.original.lastUsedAt)}</time> : "Never" },
    ...(explainIn ? [{ id: "source", header: "Where from", cell: ({ row }: { row: { original: Connection } }) =>
      <AccessExplanation projectId={explainIn} kind="connection" resourceId={row.original.id} /> } satisfies ColumnDef<typeof features, Connection>] : []),
    { id: "actions", header: "Actions", cell: ({ row }) => manage(row.original) },
  ], [manage, explainIn]);
  const table = useTable({ features, data: connections, columns, getRowId: (c) => c.id });
  return <DataTable table={table} label={label} empty={empty} />;
}

export function useConnections(scope: ListScope, projectId = "", enabled = true) {
  const { org } = useOrg();
  return useQuery({ queryKey: connectionsKey(org, scope, projectId), enabled: Boolean(org) && enabled,
    queryFn: ({ signal }) => listConnections(scope, projectId, signal) });
}

// One connection's models for a harness; shared by pickers and effort lists.
export function useConnectionModels(connectionId: string, harness: string) {
  const { org } = useOrg();
  return useQuery({ queryKey: connectionModelsKey(org, connectionId, harness), enabled: Boolean(org && connectionId),
    queryFn: ({ signal }) => listConnectionModels(connectionId, harness, signal) });
}

export function ProjectSelect({ value, onChange, idPrefix = "project" }: { value: string; onChange: (id: string) => void; idPrefix?: string }) {
  const { org } = useOrg();
  const [search, setSearch] = useState("");
  const [submitted, setSubmitted] = useState("");
  useEffect(() => { const t = window.setTimeout(() => setSubmitted(search.trim()), 250); return () => window.clearTimeout(t); }, [search]);
  const projects = useQuery({ queryKey: ["projects", org, submitted, "picker"], enabled: Boolean(org), queryFn: ({ signal }) => listProjects("", submitted, "created_at", "desc", signal) });
  return <>
    <TextField label="Find project" name={`${idPrefix}-search`} autoComplete="off" placeholder="Search projects" value={search} onChange={setSearch} onBlur={() => {}} required={false} />
    <div className="form-field"><label htmlFor={`${idPrefix}-select`}>Project</label>
      <select id={`${idPrefix}-select`} value={value} onChange={(event) => onChange(event.target.value)}>
        <option value="">Choose a project</option>
        {(projects.data?.projects ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}
      </select></div>
  </>;
}

export function NewApiKey({ scope, projectId = "", cancel, onDone }: { scope: Scope; projectId?: string; cancel: ReactNode; onDone: (c: Connection) => void }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [error, setError] = useState("");
  const [created, setCreated] = useState<Connection | null>(null);
  const [signIn, dispatch] = useSignIn();
  const abort = useRef<AbortController | null>(null);
  const form = useForm({
    defaultValues: { provider: "anthropic", apiKey: "", label: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const apiKey = value.apiKey.trim();
      if (!apiKey || new TextEncoder().encode(apiKey).length > 8192 || /[\r\n\0]/.test(apiKey) || value.label.length > 120) {
        setError("Paste a single-line API key of at most 8,192 bytes and a label of at most 120 characters.");
        return;
      }
      form.setFieldValue("apiKey", "");
      dispatch({ type: "start" });
      dispatch({ type: "verify" });
      abort.current = new AbortController();
      try {
        const connection = await createApiKeyConnection(scope, projectId, value.provider, apiKey, value.label.trim(), abort.current.signal);
        await queryClient.invalidateQueries({ queryKey: ["connections", org] });
        dispatch({ type: "succeed" });
        if (connection) setCreated(connection);
      } catch (cause) {
        dispatch({ type: "fail", error: failure(cause, "The API key could not be added. Please try again.") });
      }
    },
  });
  const cancelCheck = () => {
    dispatch({ type: "cancel" });
    abort.current?.abort();
    void queryClient.invalidateQueries({ queryKey: ["connections", org] }); // The server may have stored it already.
  };
  if (created) return <section className="editor-card" aria-labelledby="api-key-created-heading">
    <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="api-key-created-heading">{providerLabel(created.provider)} key added</h2>
      <p className={created.modelsError ? "form-field-error" : undefined}>{modelsSummary(created)}</p></div></div>
    <div className="editor-actions"><button type="button" className="primary-button" onClick={() => onDone(created)}>Continue</button></div>
  </section>;
  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="api-key-heading">
      <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="api-key-heading">Provider API key</h2><p>The platform checks the key with the provider and loads the models it can use.</p></div></div>
      <SignInStatus state={signIn} onCancel={cancelCheck} onRetry={() => dispatch({ type: "retry" })}
        labels={{ starting: "Sending the key…", verifying: "Validating the key with the provider…", succeeded: "Key accepted." }} />
      {isBusy(signIn.phase) ? null : <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
        <form.Field name="provider">{(field) => <div className="form-field"><label htmlFor="api-key-provider">Provider</label>
          <select id="api-key-provider" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}>
            {apiKeyProviders.map((p) => <option key={p.id} value={p.id}>{p.label}</option>)}
          </select></div>}</form.Field>
        <form.Field name="apiKey">{(field) => <TextField label="API key" name={field.name} type="password" autoComplete="new-password" placeholder="Paste the provider API key" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}</form.Field>
        <form.Field name="label">{(field) => <TextField label="Label (optional)" name={field.name} autoComplete="off" placeholder="e.g. Team billing key" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} required={false} />}</form.Field>
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
        <div className="editor-actions">{cancel}<form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
          {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Checking…" : "Add API key"}</button>}
        </form.Subscribe></div>
      </form>}
    </section>
    <aside className="editor-note"><h2>Write-only key</h2><p>The key is sent once and never returned. {scope === "personal" ? "A personal key serves only runs you launch." : scope === "project" ? "A project key serves only this project's runs." : "An organization key serves the projects, users, and roles you grant it to."} OpenCode Zen and OpenCode Go are OpenCode's own providers; OpenCode Go is its subscription and also uses an API key.</p></aside>
  </div>;
}

export function NewGit({ scope, projectId = "", returnTo, cancel, onDone }: { scope: Scope; projectId?: string; returnTo: string; cancel: ReactNode; onDone: (c: Connection) => void }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const app = useQuery({ queryKey: gitHubAppKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getGitHubApp(signal) });
  const [error, setError] = useState("");
  const [signIn, dispatch] = useSignIn();
  const connect = useMutation({
    mutationFn: () => startGitHubConnect(scope, projectId, returnTo),
    onMutate: () => dispatch({ type: "start" }),
    // The callback returns to returnTo with ?github=…; GitHubReturnNotice shows the outcome.
    onSuccess: (url) => { dispatch({ type: "started" }); if (url) window.location.assign(url); },
    onError: (cause) => dispatch({ type: "fail", error: failure(cause, "GitHub sign-in could not start. Please try again.") }),
  });
  const form = useForm({
    defaultValues: { host: "github.com", username: "x-access-token", token: "" },
    onSubmit: async ({ value }) => {
      setError("");
      const username = value.username.trim();
      const token = value.token.trim();
      if (!/^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$/.test(username) || !token || new TextEncoder().encode(token).length > 8192 || /[\s\0]/.test(token)) {
        setError("Enter a username of up to 128 letters, numbers, periods, underscores, or hyphens, and a single-line token of at most 8,192 bytes.");
        return;
      }
      form.setFieldValue("token", "");
      try {
        const connection = await createGitTokenConnection(scope, projectId, value.host, username, token);
        await queryClient.invalidateQueries({ queryKey: ["connections", org] });
        if (connection) onDone(connection);
      } catch (cause) {
        setError(failure(cause, "The Git connection could not be added. Please try again."));
      }
    },
  });
  const configured = app.data?.configured ?? false;
  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="git-heading">
      <div className="editor-card-heading"><span className="project-symbol"><GitBranch size={18} aria-hidden="true" /></span><div><h2 id="git-heading">Connect Git</h2><p>Sign in with GitHub, then pick repositories and branches on the project source page.</p></div></div>
      <div className="editor-form">
        {signIn.phase === "idle" ? <button type="button" className="primary-button" disabled={!configured} onClick={() => { setError(""); connect.mutate(); }}>
          <GitBranch size={15} aria-hidden="true" /> Connect GitHub</button> : null}
        <SignInStatus state={signIn} onRetry={() => dispatch({ type: "retry" })}
          labels={{ starting: "Preparing GitHub sign-in…", waiting: "Opening GitHub…", verifying: "Finishing sign-in…", succeeded: "GitHub connected." }} />
        {app.isSuccess && !configured ? <p className="admin-note">An organization owner or admin must register the GitHub OAuth App under Admin → Connections → GitHub App first.</p> : null}
        {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      </div>
      <details className="advanced-disclosure"><summary>Advanced: use a token</summary>
        <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
          <form.Field name="host">{(field) => <div className="form-field"><label htmlFor="git-host">Host</label><select id="git-host" name={field.name} value={field.state.value} onChange={(event) => field.handleChange(event.target.value)} onBlur={field.handleBlur}><option value="github.com">GitHub</option><option value="gitlab.com">GitLab</option></select></div>}</form.Field>
          <form.Field name="username">{(field) => <TextField label="Username" name={field.name} autoComplete="off" placeholder="x-access-token" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}</form.Field>
          <form.Field name="token">{(field) => <TextField label="Token" name={field.name} type="password" autoComplete="new-password" placeholder="Paste a fine-grained token" value={field.state.value} onChange={field.handleChange} onBlur={field.handleBlur} />}</form.Field>
          <div className="editor-actions">{cancel}<form.Subscribe selector={(state) => [state.canSubmit, state.isSubmitting] as const}>
            {([canSubmit, submitting]) => <button className="primary-button" type="submit" disabled={!canSubmit || submitting}>{submitting ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : <Plus size={15} aria-hidden="true" />}{submitting ? "Adding…" : "Add Git token"}</button>}
          </form.Subscribe></div>
        </form>
      </details>
    </section>
    <aside className="editor-note"><h2>Platform-only token</h2><p>The Git credential is used by the platform to fetch sources and push run branches; agents never receive it. Tokens are write-only.</p></aside>
  </div>;
}

type Pending = { kind: "use"; use: ConnectionUse } | { kind: "connection" };

// One connection's grants (organization scope), model uses, model refresh, and revocation.
export function ConnectionDetail({ connection, scope, projectId = "", onRevoked }: { connection: Connection; scope: Scope; projectId?: string; onRevoked: () => void }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [pending, setPending] = useState<Pending | null>(null);
  const [error, setError] = useState("");
  const [refreshNote, setRefreshNote] = useState("");
  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["connections", org] });
  const refresh = useMutation({
    mutationFn: () => refreshConnectionModels(connection.id),
    onSuccess: async (r) => {
      setRefreshNote(r.valid ? `valid, ${r.modelCount} ${r.modelCount === 1 ? "model" : "models"}` : r.error || "The provider rejected this connection.");
      await Promise.all([invalidate(), queryClient.invalidateQueries({ queryKey: ["connection-models", org, connection.id] })]);
    },
    onError: (cause) => setRefreshNote(failure(cause, "Models could not be refreshed.")),
  });
  const act = useMutation({
    mutationFn: async (p: Pending) => {
      if (p.kind === "use") return removeConnectionUse(p.use.id);
      return revokeConnection(connection.id);
    },
    onSuccess: async (_, p) => { setPending(null); await invalidate(); if (p.kind === "connection") onRevoked(); },
    onError: (cause) => setError(failure(cause, "The change could not be made. Please try again.")),
  });
  const uses = projectId && scope !== "organization" ? connection.uses.filter((u) => u.projectId === projectId) : connection.uses;
  const useColumns = useMemo<ColumnDef<typeof features, ConnectionUse>[]>(() => [
    { id: "project", header: "Project", cell: ({ row }) => <strong>{row.original.projectName || row.original.projectId.slice(0, 8)}</strong> },
    { id: "model", header: "Model", cell: ({ row }) => <span className="mono">{row.original.model}</span> },
    { id: "for", header: "Serves", cell: ({ row }) => row.original.granteeKind === "user" ? "Your runs only" : "Project runs" },
    { id: "created", header: "Added", cell: ({ row }) => <time dateTime={row.original.createdAt}>{ago(row.original.createdAt)}</time> },
    { id: "actions", header: "Actions", cell: ({ row }) => <button type="button" className="text-action text-action-danger" disabled={act.isPending} onClick={() => { setError(""); setPending({ kind: "use", use: row.original }); }}><Trash2 size={13} aria-hidden="true" /> Remove</button> },
  ], [act.isPending]);
  const useTableModel = useTable({ features, data: uses, columns: useColumns, getRowId: (u) => u.id });

  return <>
    <section className="table-section" aria-labelledby="connection-summary-heading">
      <div className="table-heading"><div><h2 id="connection-summary-heading">{providerLabel(connection.provider)} · {kindLabel(connection.kind)}</h2>
        <p>{scopeLabel(connection.scope)} connection{connection.ownerName ? ` owned by ${connection.ownerName}` : ""}. Account {connection.account ? <RedactedText text={connection.account} label="account" /> : "—"}. Credentials are never shown.</p></div>
        <span className="admin-actions">
          {connection.kind !== "git" && connection.canManage ? <button type="button" className="secondary-button" disabled={refresh.isPending} onClick={() => refresh.mutate()}><RefreshCw size={15} className={refresh.isPending ? "spin" : undefined} aria-hidden="true" /> Refresh models</button> : null}
          {connection.canManage && connection.state !== "revoked" ? <button type="button" className="secondary-button" onClick={() => { setError(""); setPending({ kind: "connection" }); }}><Trash2 size={15} aria-hidden="true" /> Revoke</button> : null}
        </span></div>
      <div className="source-summary"><HealthLine connection={connection} />
        <span className={connection.modelsError ? "form-field-error" : undefined}>Models: {refreshNote || modelsSummary(connection)}{connection.modelsCheckedAt ? ` · checked ${ago(connection.modelsCheckedAt)}` : ""}</span>
        <span>Last used: {connection.lastUsedAt ? ago(connection.lastUsedAt) : "never"}</span>
        {projectId ? <AccessExplanation projectId={projectId} kind="connection" resourceId={connection.id} /> : null}</div>
    </section>
    {connection.kind !== "git" ? <section className="table-section" aria-labelledby="connection-uses-heading">
      <div className="table-heading"><div><h2 id="connection-uses-heading">Model uses</h2><p>{connection.scope === "personal" ? "Projects where your own runs use this connection." : "Models project runs may use through this connection."}</p></div><span className="fetched-time">{uses.length} uses</span></div>
      <DataTable table={useTableModel} label="Model uses" empty="Not used by any project yet." />
      {connection.state === "active" ? <AddUse connection={connection} projectId={projectId} /> : null}
    </section> : null}
    {connection.kind !== "git" && connection.canManage && connection.state === "active" ? <RecommendedModels connection={connection} /> : null}
    {scope === "organization" && connection.scope === "organization" ? <ResourceGrants grants={connection.grants} label="Connection grants"
      canManage={connection.canManage} canAdd={connection.canManage && connection.state === "active"}
      description="Each grant names one project, one user, or a minimum role. Owners and admins get no implicit use; viewers never. A project grant lets that project's admins attach it to runs."
      revokeNote="Revoking a project grant also removes that project's model uses of this connection."
      grant={(kind, project, grantee) => grantConnection(connection.id, project, kind, grantee)} revoke={revokeConnectionGrant} onChanged={invalidate}
      explain={{ kind: "connection", resourceId: connection.id }} /> : null}
    {pending ? <ConfirmDialog busy={act.isPending} error={error} onClose={() => setPending(null)} onConfirm={() => act.mutate(pending)}
      title={pending.kind === "use" ? "Remove model use" : "Revoke connection"}
      confirmLabel={pending.kind === "use" ? "Remove" : "Revoke"}
      body={pending.kind === "use" ? <>Stop <strong>{pending.use.projectName}</strong> from using <strong className="mono">{pending.use.model}</strong> through this connection? Future attempts lose it; a running actor may still hold a delivered credential until stopped.</>
          : <>Revoke this {providerLabel(connection.provider)} connection? Every grant, use, and lease is revoked. Rotate the credential at the provider to invalidate copies already delivered.</>} /> : null}
  </>;
}

// Managers pin recommended models; every picker lists them first.
function RecommendedModels({ connection }: { connection: Connection }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const models = useConnectionModels(connection.id, "");
  const [error, setError] = useState("");
  const pinned = (models.data?.models ?? []).filter((m) => m.recommended).map((m) => m.id);
  const save = useMutation({
    mutationFn: (next: string[]) => setRecommendedModels(connection.id, next),
    onSuccess: async () => { setError(""); await queryClient.invalidateQueries({ queryKey: ["connection-models", org, connection.id] }); },
    onError: (cause) => setError(failure(cause, "Recommended models could not be saved.")),
  });
  const columns = useMemo<ColumnDef<typeof features, ConnectionModel>[]>(() => [
    { id: "model", header: "Model", cell: ({ row }) => <span className="task-stage"><strong>{row.original.displayName || row.original.id}</strong><small className="mono">{row.original.id}</small></span> },
    { id: "flags", header: "Status", cell: ({ row }) => [row.original.recommended ? "Recommended" : "", row.original.isDefault ? "Provider default" : "", row.original.legacy ? "Legacy" : "", row.original.badge].filter(Boolean).join(" · ") || "—" },
    { id: "efforts", header: "Efforts", cell: ({ row }) => row.original.efforts.length ? <span className="mono">{row.original.efforts.join(", ")}</span> : "Harness default" },
    { id: "actions", header: "Actions", cell: ({ row }) => <button type="button" className="text-action" disabled={save.isPending} aria-pressed={row.original.recommended}
      onClick={() => save.mutate(row.original.recommended ? pinned.filter((id) => id !== row.original.id) : [...pinned, row.original.id])}>
      <Pin size={13} aria-hidden="true" /> {row.original.recommended ? "Unpin" : "Recommend"}</button> },
  ], [pinned.join(","), save.isPending]);
  const table = useTable({ features, data: models.data?.models ?? [], columns, getRowId: (m) => m.id });
  return <section className="table-section" aria-labelledby="recommended-models-heading">
    <div className="table-heading"><div><h2 id="recommended-models-heading">Models</h2><p>Recommend models to list them first in every model picker. Legacy models stay hidden behind a toggle there.</p></div>
      <span className="fetched-time">{pinned.length} recommended</span></div>
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    <DataTable table={table} label="Connection models" empty={models.isPending ? undefined : "No models listed yet. Refresh models."} />
  </section>;
}

function AddUse({ connection, projectId }: { connection: Connection; projectId: string }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [project, setProject] = useState(projectId);
  const [model, setModel] = useState("");
  const [error, setError] = useState("");
  const add = useMutation({
    mutationFn: () => addConnectionUse(connection.id, project, model.trim()),
    onSuccess: async () => { setModel(""); setError(""); await queryClient.invalidateQueries({ queryKey: ["connections", org] }); },
    onError: (cause) => setError(failure(cause, "The model could not be added. Please try again.")),
  });
  return <form className="editor-card editor-form" noValidate onSubmit={(event) => { event.preventDefault(); if (project && model.trim()) add.mutate(); }} aria-label="Add a model use">
    {projectId ? null : <ProjectSelect value={project} onChange={setProject} />}
    <ModelSelect connectionId={connection.id} value={model} onChange={setModel} />
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    <div className="editor-actions"><button type="submit" className="primary-button" disabled={!project || !model.trim() || add.isPending}><Plus size={15} aria-hidden="true" /> {add.isPending ? "Adding…" : connection.scope === "personal" ? "Use in project" : "Add model"}</button></div>
  </form>;
}

// A standing grant as lane U stores it; ConnectionGrant is the wire shape
// for connection and recipe grants alike.
export type ResourceGrant = Pick<ConnectionGrant, "id" | "projectId" | "projectName" | "granteeKind" | "granteeId" | "granteeName" | "createdAt">;

// Grants on one organization resource (a connection or a recipe): list,
// add (project, user, or minimum role), and revoke behind a confirmation.
// The server audits both through GrantResource/RevokeResourceGrant.
export function ResourceGrants({ grants, label, description, canManage, canAdd, revokeNote = "", grant, revoke, onChanged, explain }: {
  grants: ResourceGrant[]; label: string; description: ReactNode; canManage: boolean; canAdd: boolean; revokeNote?: string;
  explain?: { kind: "connection" | "recipe"; resourceId: string };
  grant: (kind: string, projectId: string, granteeId: string) => Promise<unknown>; revoke: (grantId: string) => Promise<unknown>; onChanged: () => Promise<unknown>;
}) {
  const [pending, setPending] = useState<ResourceGrant | null>(null);
  const [error, setError] = useState("");
  const remove = useMutation({
    mutationFn: (g: ResourceGrant) => revoke(g.id),
    onSuccess: async () => { setPending(null); await onChanged(); },
    onError: (cause) => setError(failure(cause, "The grant could not be revoked. Please try again.")),
  });
  const columns = useMemo<ColumnDef<typeof features, ResourceGrant>[]>(() => [
    { id: "grantee", header: "Grantee", cell: ({ row }) => row.original.granteeKind === "project"
      ? <span className="task-stage"><strong>Project</strong><small>{row.original.projectName || row.original.projectId.slice(0, 8)}</small></span>
      : <span className="task-stage"><strong>{row.original.granteeKind === "user" ? "User" : "Minimum role"}</strong><small className="mono">{row.original.granteeName || row.original.granteeId}</small></span> },
    { id: "reach", header: "Reach", cell: ({ row }) => row.original.granteeKind === "project" ? "That project's runs and admins" : "Every project, for matching people" },
    { id: "created", header: "Granted", cell: ({ row }) => <time dateTime={row.original.createdAt}>{ago(row.original.createdAt)}</time> },
    { id: "actions", header: "Actions", cell: ({ row }) => canManage ? <button type="button" className="text-action text-action-danger" disabled={remove.isPending} onClick={() => { setError(""); setPending(row.original); }}><Trash2 size={13} aria-hidden="true" /> Revoke</button> : null },
  ], [remove.isPending, canManage]);
  const table = useTable({ features, data: grants, columns, getRowId: (g) => g.id });
  return <section className="table-section" aria-labelledby="resource-grants-heading">
    <div className="table-heading"><div><h2 id="resource-grants-heading">Grants</h2><p>{description}</p></div><span className="fetched-time">{grants.length} grants</span></div>
    <DataTable table={table} label={label} empty="Not granted to any project, user, or role." />
    {canAdd ? <AddGrant grant={grant} onChanged={onChanged} /> : null}
    {explain ? <AccessCheck kind={explain.kind} resourceId={explain.resourceId}
      projectPicker={(value, onChange) => <ProjectSelect value={value} onChange={onChange} idPrefix="explain" />} /> : null}
    {pending ? <ConfirmDialog busy={remove.isPending} error={error} onClose={() => setPending(null)} onConfirm={() => remove.mutate(pending)}
      title="Revoke grant" confirmLabel="Revoke"
      body={<>Revoke this grant for <strong>{pending.granteeKind === "project" ? pending.projectName || pending.projectId : pending.granteeName || pending.granteeId}</strong>?{revokeNote ? ` ${revokeNote}` : ""}</>} /> : null}
  </section>;
}

function AddGrant({ grant, onChanged }: { grant: (kind: string, projectId: string, granteeId: string) => Promise<unknown>; onChanged: () => Promise<unknown> }) {
  const [project, setProject] = useState("");
  const [kind, setKind] = useState("project");
  const [grantee, setGrantee] = useState("");
  const [error, setError] = useState("");
  const add = useMutation({
    mutationFn: () => grant(kind, kind === "project" ? project : "", kind === "project" ? "" : grantee.trim()),
    onSuccess: async () => { setGrantee(""); setError(""); await onChanged(); },
    onError: (cause) => setError(failure(cause, "The grant could not be added. Please try again.")),
  });
  const ready = kind === "project" ? Boolean(project) : Boolean(grantee.trim());
  return <form className="editor-card editor-form" noValidate onSubmit={(event) => { event.preventDefault(); if (ready) add.mutate(); }} aria-label="Add a grant">
    <div className="form-field"><label htmlFor="grantee-kind">Grant to</label>
      <select id="grantee-kind" value={kind} onChange={(event) => { setKind(event.target.value); setGrantee(event.target.value === "role" ? "member" : ""); }}>
        <option value="project">A project</option><option value="user">A user (all projects)</option><option value="role">A minimum role (all projects)</option>
      </select></div>
    {kind === "project" ? <ProjectSelect value={project} onChange={setProject} /> : null}
    {kind === "user" ? <TextField label="User principal id" name="grantee-user" autoComplete="off" placeholder="Principal id" value={grantee} onChange={setGrantee} onBlur={() => {}} /> : null}
    {kind === "role" ? <div className="form-field"><label htmlFor="grantee-role">Role</label>
      <select id="grantee-role" value={grantee} onChange={(event) => setGrantee(event.target.value)}>
        {[["member", "Members and above"], ["admin", "Admins and owners"], ["owner", "Owners only"]].map(([r, name]) => <option key={r} value={r}>{name}</option>)}
      </select></div> : null}
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    <div className="editor-actions"><button type="submit" className="primary-button" disabled={!ready || add.isPending}><Plus size={15} aria-hidden="true" /> {add.isPending ? "Granting…" : "Add grant"}</button></div>
  </form>;
}

// ?github=connected|error&message=… is set by the /oauth/github/callback redirect.
// It ends the OAuth sign-in state machine started by NewGit's Connect GitHub.
export function GitHubReturnNotice({ search }: { search: Record<string, unknown> }) {
  const navigate = useNavigate();
  const state: SignInState | null = search.github === "connected" ? { phase: "succeeded", attempt: 1 }
    : search.github === "error" ? { phase: "failed", attempt: 1, error: typeof search.message === "string" ? search.message : "Please try again." } : null;
  if (!state) return null;
  return <SignInStatus state={state} onRetry={() => void navigate({ to: ".", search: {} as never, replace: true })}
    labels={{ starting: "", verifying: "", succeeded: "GitHub connected." }} done="Pick repositories on a project's source page." />;
}
