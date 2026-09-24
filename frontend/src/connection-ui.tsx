import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { GitBranch, KeyRound, Plus, RefreshCw, Trash2 } from "lucide-react";
import { ago } from "./admin";
import { currentSession, sessionQueryKey } from "./auth";
import {
  addConnectionUse, apiKeyProviders, connectionModelsKey, connectionsKey, createApiKeyConnection, createGitTokenConnection,
  getGitHubApp, gitHubAppKey, grantConnection, kindLabel, listConnectionModels, listConnections, modelsSummary, providerLabel,
  refreshConnectionModels, removeConnectionUse, revokeConnection, revokeConnectionGrant, scopeLabel, startGitHubConnect,
  type ListScope, type Scope,
} from "./connections";
import { DataTable } from "./data-table";
import { TextField } from "./form-field";
import type { Connection, ConnectionGrant, ConnectionUse } from "./gen/blaxsmith/api/v1/connections_pb";
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

export function ConnectionTable({ connections, label, empty, manage }: {
  connections: Connection[]; label: string; empty: string; manage: (c: Connection) => ReactNode;
}) {
  const columns = useMemo<ColumnDef<typeof features, Connection>[]>(() => [
    { id: "provider", header: "Provider", cell: ({ row }) => <span className="task-stage"><strong>{providerLabel(row.original.provider)}</strong><small>{kindLabel(row.original.kind)}{row.original.label ? ` · ${row.original.label}` : ""}</small></span> },
    { id: "scope", header: "Scope / owner", cell: ({ row }) => <span className="task-stage"><strong>{scopeLabel(row.original.scope)}</strong><small>{row.original.ownerName || row.original.ownerId.slice(0, 8)}</small></span> },
    { id: "account", header: "Account", cell: ({ row }) => <span className="mono admin-wrap">{row.original.account || "—"}</span> },
    { id: "state", header: "State", cell: ({ row }) => <StateBadge state={row.original.state} /> },
    { id: "models", header: "Models", cell: ({ row }) => <span className={row.original.modelsError ? "form-field-error" : undefined}>{modelsSummary(row.original)}</span> },
    { id: "grants", header: "Grants / uses", cell: ({ row }) => `${row.original.grants.length} / ${row.original.uses.length}` },
    { id: "used", header: "Last used", cell: ({ row }) => row.original.lastUsedAt ? <time dateTime={row.original.lastUsedAt}>{ago(row.original.lastUsedAt)}</time> : "Never" },
    { id: "actions", header: "Actions", cell: ({ row }) => manage(row.original) },
  ], [manage]);
  const table = useTable({ features, data: connections, columns, getRowId: (c) => c.id });
  return <DataTable table={table} label={label} empty={empty} />;
}

export function useConnections(scope: ListScope, projectId = "", enabled = true) {
  const { org } = useOrg();
  return useQuery({ queryKey: connectionsKey(org, scope, projectId), enabled: Boolean(org) && enabled,
    queryFn: ({ signal }) => listConnections(scope, projectId, signal) });
}

const harnesses = [["", "Any harness"], ["codex", "Codex"], ["claude-code", "Claude Code"], ["opencode", "OpenCode"]] as const;
const modelId = /^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$/;

// Models the connection can actually use; free text only under Advanced.
// harness fixes the harness filter (a recipe profile already chose one);
// without a connection only the Advanced free-text field is offered. fieldId
// keeps element ids unique when several selects share a connection.
export function ModelSelect({ connectionId, value, onChange, harness: fixedHarness, fieldId, error }: {
  connectionId: string; value: string; onChange: (model: string) => void; harness?: string; fieldId?: string; error?: string;
}) {
  const { org } = useOrg();
  const [chosenHarness, setHarness] = useState("");
  const harness = fixedHarness ?? chosenHarness;
  const [search, setSearch] = useState("");
  const models = useQuery({ queryKey: connectionModelsKey(org, connectionId, harness), enabled: Boolean(org && connectionId),
    queryFn: ({ signal }) => listConnectionModels(connectionId, harness, signal) });
  const list = connectionId ? models.data?.models ?? [] : [];
  const needle = search.trim().toLowerCase();
  const shown = needle ? list.filter((m) => m.id.toLowerCase().includes(needle) || m.displayName.toLowerCase().includes(needle)) : list;
  const listed = list.some((m) => m.id === value);
  const id = fieldId || connectionId || "none";
  return <div className="editor-form">
    {connectionId && fixedHarness === undefined ? <div className="form-field"><label htmlFor={`harness-${id}`}>Harness</label>
      <select id={`harness-${id}`} value={harness} onChange={(event) => setHarness(event.target.value)}>
        {harnesses.map(([hid, name]) => <option key={hid} value={hid}>{name}</option>)}
      </select></div> : null}
    {list.length > 12 ? <TextField label="Search models" name={`model-search-${id}`} autoComplete="off" placeholder="Filter by id or name" value={search} onChange={setSearch} onBlur={() => {}} required={false} /> : null}
    {connectionId ? <div className="form-field"><label htmlFor={`model-${id}`}>Model</label>
      <select id={`model-${id}`} value={listed ? value : ""} onChange={(event) => onChange(event.target.value)} disabled={models.isPending} aria-invalid={error ? true : undefined}>
        <option value="">{models.isPending ? "Loading models…" : list.length ? "Choose a model" : "No models available"}</option>
        {shown.map((m) => <option key={m.id} value={m.id}>{m.displayName && m.displayName !== m.id ? `${m.displayName} (${m.id})` : m.id}{m.contextTokens ? ` · ${Math.round(m.contextTokens / 1000)}k` : ""}</option>)}
      </select>
      {models.data?.error ? <span className="form-field-error">{models.data.error}</span> : null}
      {models.isError ? <span className="form-field-error">Models could not be loaded.</span> : null}
      {error && listed ? <span className="form-field-error">{error}</span> : null}
    </div> : null}
    <details className="advanced-disclosure" open={!connectionId || (Boolean(value) && !listed && !models.isPending) || undefined}><summary>Advanced: type a model id</summary>
      <TextField label="Model id" name={`model-free-text-${id}`} autoComplete="off" placeholder="Exact provider model id" value={listed ? "" : value} onChange={onChange} onBlur={() => {}} required={false}
        error={value && !listed && !modelId.test(value) ? "Use up to 128 letters, numbers, periods, underscores, slashes, or hyphens." : !listed ? error : undefined} />
    </details>
  </div>;
}

export function ProjectSelect({ value, onChange }: { value: string; onChange: (id: string) => void }) {
  const { org } = useOrg();
  const [search, setSearch] = useState("");
  const [submitted, setSubmitted] = useState("");
  useEffect(() => { const t = window.setTimeout(() => setSubmitted(search.trim()), 250); return () => window.clearTimeout(t); }, [search]);
  const projects = useQuery({ queryKey: ["projects", org, submitted, "picker"], enabled: Boolean(org), queryFn: ({ signal }) => listProjects("", submitted, "created_at", "desc", signal) });
  return <>
    <TextField label="Find project" name="project-search" autoComplete="off" placeholder="Search projects" value={search} onChange={setSearch} onBlur={() => {}} required={false} />
    <div className="form-field"><label htmlFor="project-select">Project</label>
      <select id="project-select" value={value} onChange={(event) => onChange(event.target.value)}>
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
      try {
        const connection = await createApiKeyConnection(scope, projectId, value.provider, apiKey, value.label.trim());
        await queryClient.invalidateQueries({ queryKey: ["connections", org] });
        if (connection) setCreated(connection);
      } catch (cause) {
        setError(failure(cause, "The API key could not be added. Please try again."));
      }
    },
  });
  if (created) return <section className="editor-card" aria-labelledby="api-key-created-heading">
    <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="api-key-created-heading">{providerLabel(created.provider)} key added</h2>
      <p className={created.modelsError ? "form-field-error" : undefined}>{modelsSummary(created)}</p></div></div>
    <div className="editor-actions"><button type="button" className="primary-button" onClick={() => onDone(created)}>Continue</button></div>
  </section>;
  return <div className="editor-layout">
    <section className="editor-card" aria-labelledby="api-key-heading">
      <div className="editor-card-heading"><span className="project-symbol"><KeyRound size={18} aria-hidden="true" /></span><div><h2 id="api-key-heading">Provider API key</h2><p>The platform checks the key with the provider and loads the models it can use.</p></div></div>
      <form className="editor-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
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
      </form>
    </section>
    <aside className="editor-note"><h2>Write-only key</h2><p>The key is sent once and never returned. {scope === "personal" ? "A personal key serves only runs you launch." : scope === "project" ? "A project key serves only this project's runs." : "An organization key serves the projects, users, and roles you grant it to."} OpenCode Zen and OpenCode Go are OpenCode's own providers; OpenCode Go is its subscription and also uses an API key.</p></aside>
  </div>;
}

export function NewGit({ scope, projectId = "", returnTo, cancel, onDone }: { scope: Scope; projectId?: string; returnTo: string; cancel: ReactNode; onDone: (c: Connection) => void }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const app = useQuery({ queryKey: gitHubAppKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getGitHubApp(signal) });
  const [error, setError] = useState("");
  const connect = useMutation({
    mutationFn: () => startGitHubConnect(scope, projectId, returnTo),
    onSuccess: (url) => { if (url) window.location.assign(url); },
    onError: (cause) => setError(failure(cause, "GitHub sign-in could not start. Please try again.")),
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
        <button type="button" className="primary-button" disabled={!configured || connect.isPending} onClick={() => { setError(""); connect.mutate(); }}>
          <GitBranch size={15} aria-hidden="true" /> {connect.isPending ? "Opening GitHub…" : "Connect GitHub"}</button>
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
        <p>{scopeLabel(connection.scope)} connection{connection.ownerName ? ` owned by ${connection.ownerName}` : ""}. Account <span className="mono">{connection.account || "—"}</span>. Credentials are never shown.</p></div>
        <span className="admin-actions">
          {connection.kind !== "git" && connection.canManage ? <button type="button" className="secondary-button" disabled={refresh.isPending} onClick={() => refresh.mutate()}><RefreshCw size={15} className={refresh.isPending ? "spin" : undefined} aria-hidden="true" /> Refresh models</button> : null}
          {connection.canManage && connection.state !== "revoked" ? <button type="button" className="secondary-button" onClick={() => { setError(""); setPending({ kind: "connection" }); }}><Trash2 size={15} aria-hidden="true" /> Revoke</button> : null}
        </span></div>
      <div className="source-summary"><strong><StateBadge state={connection.state} /></strong>
        <span className={connection.modelsError ? "form-field-error" : undefined}>Models: {refreshNote || modelsSummary(connection)}{connection.modelsCheckedAt ? ` · checked ${ago(connection.modelsCheckedAt)}` : ""}</span>
        <span>Last used: {connection.lastUsedAt ? ago(connection.lastUsedAt) : "never"}</span></div>
    </section>
    {connection.kind !== "git" ? <section className="table-section" aria-labelledby="connection-uses-heading">
      <div className="table-heading"><div><h2 id="connection-uses-heading">Model uses</h2><p>{connection.scope === "personal" ? "Projects where your own runs use this connection." : "Models project runs may use through this connection."}</p></div><span className="fetched-time">{uses.length} uses</span></div>
      <DataTable table={useTableModel} label="Model uses" empty="Not used by any project yet." />
      {connection.state === "active" ? <AddUse connection={connection} projectId={projectId} /> : null}
    </section> : null}
    {scope === "organization" && connection.scope === "organization" ? <ResourceGrants grants={connection.grants} label="Connection grants"
      canManage={connection.canManage} canAdd={connection.canManage && connection.state === "active"}
      description="Each grant names one project, one user, or a minimum role. Owners and admins get no implicit use; viewers never. A project grant lets that project's admins attach it to runs."
      revokeNote="Revoking a project grant also removes that project's model uses of this connection."
      grant={(kind, project, grantee) => grantConnection(connection.id, project, kind, grantee)} revoke={revokeConnectionGrant} onChanged={invalidate} /> : null}
    {pending ? <ConfirmDialog busy={act.isPending} error={error} onClose={() => setPending(null)} onConfirm={() => act.mutate(pending)}
      title={pending.kind === "use" ? "Remove model use" : "Revoke connection"}
      confirmLabel={pending.kind === "use" ? "Remove" : "Revoke"}
      body={pending.kind === "use" ? <>Stop <strong>{pending.use.projectName}</strong> from using <strong className="mono">{pending.use.model}</strong> through this connection? Future attempts lose it; a running actor may still hold a delivered credential until stopped.</>
          : <>Revoke this {providerLabel(connection.provider)} connection? Every grant, use, and lease is revoked. Rotate the credential at the provider to invalidate copies already delivered.</>} /> : null}
  </>;
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
export function ResourceGrants({ grants, label, description, canManage, canAdd, revokeNote = "", grant, revoke, onChanged }: {
  grants: ResourceGrant[]; label: string; description: ReactNode; canManage: boolean; canAdd: boolean; revokeNote?: string;
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
export function GitHubReturnNotice({ search }: { search: Record<string, unknown> }) {
  if (search.github === "connected") return <div className="notice" role="status"><strong>GitHub connected.</strong> Pick repositories on a project's source page.</div>;
  if (search.github === "error") return <div className="notice" role="alert"><strong>GitHub connection failed.</strong> {typeof search.message === "string" ? search.message : "Please try again."}</div>;
  return null;
}
