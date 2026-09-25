// Connection collections (with an expandable models/grants summary per row)
// and the connection detail page: Overview · Models · Grants · Usage · Activity.
import { useMemo, useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useLocation } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { ArrowRight, Pin, RefreshCw, Trash2 } from "lucide-react";
import { AccessExplanation } from "./access-explain";
import { ago } from "./admin";
import { AddUse, ConfirmDialog, failure, HealthLine, RedactedText, ResourceGrants, StateBadge, useOrg } from "./connection-ui";
import {
  connectionModelsKey, connectionTitle, grantConnection, granteeLabel, kindLabel, listConnectionModels, modelsSummary, providerLabel, refreshConnectionModels,
  removeConnectionUse, revokeConnection, revokeConnectionGrant, scopeLabel, setRecommendedModels, type Scope,
} from "./connections";
import { CollectionTable, DataTable, inSet, useLocalView, useUrlView, type GridColumn } from "./data-table";
import type { Connection, ConnectionModel, ConnectionUse } from "./gen/blaxsmith/api/v1/connections_pb";
import { DetailLayout, SummaryList } from "./layouts";
import { Slot } from "./slots";
import { Card, CopyValue, Disclosure, EmptyState, tabFrom, Timestamp, type TabSpec } from "./ui";
import { useScope } from "./workspace-ui";

const providerOptions = (rows: Connection[]) => [...new Set(rows.map((c) => c.provider))].map((p) => ({ value: p, label: providerLabel(p) }));
const stateOptions = [{ value: "active", label: "Active" }, { value: "reconnect_required", label: "Reconnect required" }, { value: "disabled", label: "Disabled" }, { value: "revoked", label: "Revoked" }];
const kindOptions = [{ value: "api_key", label: "API key" }, { value: "git", label: "Git" }, { value: "subscription", label: "Subscription" }];

// explainIn adds a "Where from" column: the access chain in that project.
export function ConnectionCollection({ id, label, connections, empty, href, action = "Manage", urlState = false, explainIn }: {
  id: string; label: string; connections: Connection[]; empty: ReactNode; href: (c: Connection) => string; action?: string; urlState?: boolean; explainIn?: string;
}) {
  const url = useUrlView({ size: 20 });
  const local = useLocalView({ size: 20 });
  const [view, setView] = urlState ? url : local;
  const columns = useMemo<GridColumn<Connection>[]>(() => [
    { id: "provider", accessorFn: (c) => c.provider, header: "Provider", enableHiding: false, filterFn: inSet, cell: ({ row }) => <Link className="row-link" to={href(row.original) as "/"}>
      <span className="task-stage"><strong>{connectionTitle(row.original)}</strong><small>{kindLabel(row.original.kind)}</small></span></Link> },
    { id: "kind", accessorKey: "kind", header: "Kind", filterFn: inSet, cell: ({ row }) => kindLabel(row.original.kind) },
    { id: "scope", accessorKey: "scope", header: "Scope", filterFn: inSet, cell: ({ row }) => <span className="task-stage"><strong>{scopeLabel(row.original.scope)}</strong><small>{row.original.ownerName || row.original.ownerId.slice(0, 8)}</small></span> },
    { id: "account", accessorKey: "account", header: "Account", cell: ({ row }) => row.original.account ? <span className="mono"><RedactedText text={row.original.account} label="account" /></span> : "—" },
    { id: "state", accessorKey: "state", header: "Health", filterFn: inSet, cell: ({ row }) => <HealthLine connection={row.original} compact /> },
    { id: "models", accessorKey: "modelCount", header: "Models", cell: ({ row }) => <span className={row.original.modelsError ? "form-field-error" : undefined}>{row.original.kind === "git" ? "—" : modelsSummary(row.original)}</span> },
    { id: "grants", accessorFn: (c) => c.grants.length + c.uses.length, header: "Grants · uses", cell: ({ row }) => `${row.original.grants.length} · ${row.original.uses.length}` },
    { id: "used", accessorKey: "lastUsedAt", header: "Last used", cell: ({ row }) => row.original.lastUsedAt ? <Timestamp value={row.original.lastUsedAt} /> : <span className="muted">Never</span> },
    ...(explainIn ? [{ id: "source", header: "Where from", enableSorting: false, cell: ({ row }) => <AccessExplanation projectId={explainIn} kind="connection" resourceId={row.original.id} /> } satisfies GridColumn<Connection>] : []),
    { id: "action", header: "", enableSorting: false, enableHiding: false, cell: ({ row }) => <Link className="text-action" to={href(row.original) as "/"}>{action} <ArrowRight size={13} aria-hidden="true" /></Link> },
  ], [href, action, explainIn]);
  return <CollectionTable id={id} label={label} noun="connections" columns={columns} data={connections} getRowId={(c) => c.id} view={view} onView={setView} pinFirst
    searchLabel="Search connections" facets={[{ id: "provider", label: "Provider", options: providerOptions(connections) }, { id: "kind", label: "Kind", options: kindOptions },
      { id: "state", label: "State", options: stateOptions }, ...(new Set(connections.map((c) => c.scope)).size > 1 ? [{ id: "scope", label: "Scope", options: [{ value: "organization", label: "Organization" }, { value: "project", label: "Project" }, { value: "personal", label: "Personal" }] }] : [])]}
    renderExpanded={(c) => <ConnectionRowSummary connection={c} href={href(c)} />} expandLabel={(c) => `${providerLabel(c.provider)} ${c.account}`}
    empty={empty} />;
}

function ConnectionRowSummary({ connection: c, href }: { connection: Connection; href: string }) {
  return <div className="row-summary">
    <span><strong>Models</strong> {c.kind === "git" ? "Git connections carry no models." : modelsSummary(c)}{c.modelsCheckedAt ? ` · checked ${ago(c.modelsCheckedAt)}` : ""}</span>
    <span><strong>Uses</strong> {c.uses.length ? c.uses.slice(0, 3).map((u) => `${u.projectName || "project"} → ${u.model}`).join(", ") + (c.uses.length > 3 ? ` +${c.uses.length - 3}` : "") : "none"}</span>
    <span><strong>Grants</strong> {c.grants.length ? c.grants.slice(0, 3).map((g) => granteeLabel(g)).join(", ") + (c.grants.length > 3 ? ` +${c.grants.length - 3}` : "") : "none"}</span>
    <Link className="text-action" to={href as "/"}>Open <ArrowRight size={13} aria-hidden="true" /></Link>
  </div>;
}

// Distinct projects with a model use, at most three, for the access chain.
const projectsUsing = (c: Connection) => [...new Map(c.uses.map((u) => [u.projectId, { id: u.projectId, name: u.projectName || "Project" }])).values()].slice(0, 3);

const connectionTabs = (c: Connection, scope: Scope): TabSpec[] => [
  { id: "overview", label: "Overview" },
  { id: "models", label: "Models", count: c.modelCount, hidden: c.kind === "git" },
  { id: "grants", label: "Grants", count: c.grants.length, hidden: !(scope === "organization" && c.scope === "organization") },
  { id: "usage", label: "Usage", count: c.uses.length, hidden: c.kind === "git" },
  { id: "activity", label: "Activity" },
];

// canAdminister: whether the caller administers projectId (GetProject says);
// adding or removing project model uses of shared connections needs it.
export function ConnectionDetailPage({ connection, scope, projectId = "", canAdminister = true, back, onRevoked }: {
  connection: Connection; scope: Scope; projectId?: string; canAdminister?: boolean; back: { href: string; label: string }; onRevoked: () => void;
}) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const { isAdmin } = useScope();
  const search = useLocation({ select: (l) => l.search as Record<string, unknown> });
  const tabs = connectionTabs(connection, scope);
  const tab = tabFrom(search, tabs);
  const [revoking, setRevoking] = useState(false);
  const [error, setError] = useState("");
  const invalidate = () => queryClient.invalidateQueries({ queryKey: ["connections", org] });
  const revoke = useMutation({
    mutationFn: () => revokeConnection(connection.id),
    onSuccess: async () => { setRevoking(false); await invalidate(); onRevoked(); },
    onError: (cause) => setError(failure(cause, "The connection could not be revoked. Please try again.")),
  });
  const base = useLocation({ select: (l) => l.pathname });
  const tabLink = (id: string, label: string) => <Link to={base as "/"} search={{ tab: id } as never} className="text-action">{label} <ArrowRight size={13} aria-hidden="true" /></Link>;
  const c = connection;
  return <DetailLayout back={back} title={connectionTitle(c)} status={<StateBadge state={c.state} />}
    facts={[
      { label: "Kind", value: kindLabel(c.kind) },
      { label: "Scope", value: `${scopeLabel(c.scope)}${c.ownerName ? ` · ${c.ownerName}` : ""}` },
      { label: "Account", value: c.account ? <span className="mono"><RedactedText text={c.account} label="account" /></span> : "—" },
      { label: "Last used", value: c.lastUsedAt ? <Timestamp value={c.lastUsedAt} /> : "Never" },
    ]}
    slot={<Slot name="connection.health" connectionId={c.id} scope={scope} projectId={projectId || undefined} />}
    actions={c.canManage && c.state !== "revoked" ? <button type="button" className="secondary-button danger-outline" onClick={() => { setError(""); setRevoking(true); }}><Trash2 size={15} aria-hidden="true" /> Revoke</button> : undefined}
    tabs={tabs} current={tab} tabsLabel="Connection sections">
    {tab === "overview" ? <div className="dash-grid">
      <Card title="Summary" className="dash-main" description="Credentials are write-only and never shown.">
        <SummaryList items={[
          { label: "Provider", value: providerLabel(c.provider) },
          { label: "Owner", value: `${scopeLabel(c.scope)}${c.ownerName ? ` · ${c.ownerName}` : ""}` },
          { label: "Created", value: <Timestamp value={c.createdAt} /> },
          ...(c.kind !== "git" ? [{ label: "Models", value: <span className={c.modelsError ? "form-field-error" : undefined}>{modelsSummary(c)}</span> }] : []),
        ]} />
        <div className="card-body"><Disclosure summary="Advanced: identifiers">
          <SummaryList items={[{ label: "Connection ID", value: <CopyValue value={c.id} label="Connection ID" chars={13} /> }, { label: "Owner ID", value: <CopyValue value={c.ownerId} label="Owner ID" chars={13} /> }]} />
        </Disclosure></div>
      </Card>
      <Card title="Where this comes from" className="dash-main" description={projectId ? "Why runs in this project may use it." : "Why each project that uses it is allowed to."}>
        <div className="card-body access-list">
          {projectId ? <AccessExplanation projectId={projectId} kind="connection" resourceId={c.id} />
            : projectsUsing(c).length ? projectsUsing(c).map((u) => <div key={u.id}><strong>{u.name}</strong><AccessExplanation projectId={u.id} kind="connection" resourceId={c.id} /></div>)
            : <p className="muted">No project uses it yet.{!tabs.find((t) => t.id === "grants")?.hidden ? <> Check a project and member on the {tabLink("grants", "Grants tab")}</> : null}</p>}
        </div>
      </Card>
      <Card title="Where it is used" className="dash-side">
        <ul className="link-list">
          {c.kind !== "git" ? <li>{tabLink("models", `${c.modelCount} ${c.modelCount === 1 ? "model" : "models"} available`)}</li> : null}
          {!tabs.find((t) => t.id === "grants")?.hidden ? <li>{tabLink("grants", `${c.grants.length} ${c.grants.length === 1 ? "grant" : "grants"}`)}</li> : null}
          {c.kind !== "git" ? <li>{tabLink("usage", `${c.uses.length} model ${c.uses.length === 1 ? "use" : "uses"} in projects`)}</li> : null}
          <li>{tabLink("activity", "Recent activity")}</li>
        </ul>
      </Card>
    </div> : null}
    {tab === "models" ? <ModelsTab connection={c} /> : null}
    {tab === "grants" ? <ResourceGrants grants={c.grants} label="Connection grants" canManage={c.canManage} canAdd={c.canManage && c.state === "active"} explain={{ kind: "connection", resourceId: c.id }}
      description="Each grant names one project, one user, or a minimum role. Owners and admins get no implicit use; viewers never. A project grant lets that project's admins attach it to runs."
      revokeNote="Revoking a project grant also removes that project's model uses of this connection."
      grant={(kind, project, grantee) => grantConnection(c.id, project, kind, grantee)} revoke={revokeConnectionGrant} onChanged={invalidate} /> : null}
    {tab === "usage" ? <UsageTab connection={c} projectId={projectId} scope={scope} canChange={c.scope === "personal" || canAdminister} /> : null}
    {tab === "activity" ? <Card title="Activity" description={isAdmin ? "Derived from this connection's recorded times. The complete history is in the audit log." : "Derived from this connection's recorded times."}
      actions={isAdmin ? <Link className="text-action" to="/admin/audit">Audit log <ArrowRight size={13} aria-hidden="true" /></Link> : undefined}>
      <ol className="timeline">{[
        { at: c.createdAt, text: "Connection added" },
        ...c.grants.map((g) => ({ at: g.createdAt, text: `Granted to ${granteeLabel(g, " and above")}` })),
        ...c.uses.map((u) => ({ at: u.createdAt, text: `${u.projectName || "A project"} started using ${u.model}` })),
        ...(c.modelsCheckedAt ? [{ at: c.modelsCheckedAt, text: c.modelsError ? `Model check failed: ${c.modelsError}` : `Models checked: ${c.modelCount}` }] : []),
        ...(c.lastUsedAt ? [{ at: c.lastUsedAt, text: "Last leased to a run" }] : []),
      ].filter((e) => e.at).sort((a, b) => b.at.localeCompare(a.at)).map((e, i) => <li key={`${e.at}-${i}`}><span>{e.text}</span><Timestamp value={e.at} /></li>)}</ol>
    </Card> : null}
    {revoking ? <ConfirmDialog busy={revoke.isPending} error={error} onClose={() => setRevoking(false)} onConfirm={() => revoke.mutate()} title="Revoke connection" confirmLabel="Revoke"
      body={<>Revoke this {providerLabel(c.provider)} connection? Every grant, use, and lease is revoked. Rotate the credential at the provider to invalidate copies already delivered.</>} /> : null}
  </DetailLayout>;
}

function ModelsTab({ connection }: { connection: Connection }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [view, setView] = useLocalView({ size: 20 });
  const [note, setNote] = useState("");
  const models = useQuery({ queryKey: connectionModelsKey(org, connection.id, ""), enabled: Boolean(org), queryFn: ({ signal }) => listConnectionModels(connection.id, "", signal) });
  const refresh = useMutation({
    mutationFn: () => refreshConnectionModels(connection.id),
    onSuccess: async (r) => {
      setNote(r.valid ? `Valid, ${r.modelCount} ${r.modelCount === 1 ? "model" : "models"}.` : r.error || "The provider rejected this connection.");
      await Promise.all([queryClient.invalidateQueries({ queryKey: ["connections", org] }), queryClient.invalidateQueries({ queryKey: ["connection-models", org, connection.id] })]);
    },
    onError: (cause) => setNote(failure(cause, "Models could not be refreshed.")),
  });
  // Managers pin recommended models; every model picker lists them first.
  const pinned = (models.data?.models ?? []).filter((m) => m.recommended).map((m) => m.id);
  const recommend = useMutation({
    mutationFn: (next: string[]) => setRecommendedModels(connection.id, next),
    onSuccess: async () => { setNote(""); await queryClient.invalidateQueries({ queryKey: ["connection-models", org, connection.id] }); },
    onError: (cause) => setNote(failure(cause, "Recommended models could not be saved.")),
  });
  const manage = connection.canManage && connection.state === "active";
  const columns = useMemo<GridColumn<ConnectionModel>[]>(() => [
    { id: "id", accessorKey: "id", header: "Model", enableHiding: false, cell: ({ row }) => <span className="task-stage"><strong className="mono">{row.original.id}</strong>{row.original.displayName && row.original.displayName !== row.original.id ? <small>{row.original.displayName}</small> : null}</span> },
    { id: "status", accessorFn: (m) => (m.recommended ? 0 : 1), header: "Status", cell: ({ row }) => [row.original.recommended ? "Recommended" : "", row.original.isDefault ? "Provider default" : "", row.original.legacy ? "Legacy" : "", row.original.badge].filter(Boolean).join(" · ") || "—" },
    { id: "efforts", accessorFn: (m) => m.efforts.join(","), header: "Efforts", enableSorting: false, cell: ({ row }) => row.original.efforts.length ? <span className="mono">{row.original.efforts.join(", ")}{row.original.defaultEffort ? ` (default ${row.original.defaultEffort})` : ""}</span> : "Harness default" },
    { id: "harnesses", accessorFn: (m) => m.harnesses, header: "Harnesses", filterFn: inSet, enableSorting: false, cell: ({ row }) => row.original.harnesses.join(", ") || "—" },
    { id: "context", accessorKey: "contextTokens", header: "Context", cell: ({ row }) => row.original.contextTokens ? `${Math.round(row.original.contextTokens / 1000)}k tokens` : "—" },
    { id: "created", accessorKey: "createdAt", header: "Released", cell: ({ row }) => row.original.createdAt ? <Timestamp value={row.original.createdAt} /> : "—" },
    ...(manage ? [{ id: "recommend", header: "", enableSorting: false, enableHiding: false, cell: ({ row }) => <button type="button" className="text-action" disabled={recommend.isPending} aria-pressed={row.original.recommended}
      onClick={() => recommend.mutate(row.original.recommended ? pinned.filter((id) => id !== row.original.id) : [...pinned, row.original.id])}>
      <Pin size={13} aria-hidden="true" /> {row.original.recommended ? "Unpin" : "Recommend"}</button> } satisfies GridColumn<ConnectionModel>] : []),
  ], [manage, pinned.join(","), recommend.isPending]);
  const harnessOptions = [...new Set((models.data?.models ?? []).flatMap((m) => m.harnesses))].map((h) => ({ value: h, label: h }));
  return <section className="table-section" aria-labelledby="models-heading">
    <div className="table-heading"><div><h2 id="models-heading">Models</h2><p>{models.data?.error ? <span className="form-field-error">{models.data.error}</span> : note || `Models this connection can use${models.data?.checkedAt ? `, checked ${ago(models.data.checkedAt)}` : ""}. ${pinned.length} recommended; pickers list recommended models first and hide legacy ones behind a toggle.`}</p></div>
      {connection.canManage ? <button type="button" className="secondary-button" disabled={refresh.isPending} onClick={() => refresh.mutate()}><RefreshCw size={15} className={refresh.isPending ? "spin" : undefined} aria-hidden="true" /> Refresh models</button> : null}</div>
    <CollectionTable id="connection-models" label="Models" noun="models" columns={columns} data={models.data?.models ?? []} getRowId={(m) => m.id} view={view} onView={setView}
      searchLabel="Search models" facets={harnessOptions.length ? [{ id: "harnesses", label: "Harness", options: harnessOptions }] : []}
      renderExpanded={(m) => m.capabilitiesJson ? <Disclosure summary="Advanced: provider capabilities (raw JSON)"><pre className="code-block">{m.capabilitiesJson}</pre></Disclosure> : <p className="row-detail muted">The provider returned no capability details.</p>}
      expandLabel={(m) => m.id} loading={models.isPending}
      error={models.isError ? <>Models could not be loaded. <button type="button" className="text-action" onClick={() => void models.refetch()}>Try again</button></> : undefined}
      empty={<EmptyState title="No models listed">{connection.canManage ? "Refresh to ask the provider again." : "Ask the connection owner to refresh it."}</EmptyState>} />
  </section>;
}

const useFeatures = tableFeatures({});

function UsageTab({ connection, projectId, scope, canChange }: { connection: Connection; projectId: string; scope: Scope; canChange: boolean }) {
  const queryClient = useQueryClient();
  const { org } = useOrg();
  const [pending, setPending] = useState<ConnectionUse | null>(null);
  const [error, setError] = useState("");
  const remove = useMutation({
    mutationFn: (u: ConnectionUse) => removeConnectionUse(u.id),
    onSuccess: async () => { setPending(null); await queryClient.invalidateQueries({ queryKey: ["connections", org] }); },
    onError: (cause) => setError(failure(cause, "The model use could not be removed. Please try again.")),
  });
  const uses = projectId && scope !== "organization" ? connection.uses.filter((u) => u.projectId === projectId) : connection.uses;
  const columns = useMemo<ColumnDef<typeof useFeatures, ConnectionUse>[]>(() => [
    { id: "project", header: "Project", cell: ({ row }) => <strong>{row.original.projectName || row.original.projectId.slice(0, 8)}</strong> },
    { id: "model", header: "Model", cell: ({ row }) => <span className="mono">{row.original.model}</span> },
    { id: "for", header: "Serves", cell: ({ row }) => row.original.granteeKind === "user" ? "Your runs only" : "Project runs" },
    { id: "created", header: "Added", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
    ...(!canChange ? [] : [{ id: "actions", header: "Actions", cell: ({ row }) => <button type="button" className="text-action text-action-danger" disabled={remove.isPending} onClick={() => { setError(""); setPending(row.original); }}><Trash2 size={13} aria-hidden="true" /> Remove</button> } satisfies ColumnDef<typeof useFeatures, ConnectionUse>]),
  ], [remove.isPending, canChange]);
  const table = useTable({ features: useFeatures, data: uses, columns, getRowId: (u) => u.id });
  return <section className="table-section" aria-labelledby="uses-heading">
    <div className="table-heading"><div><h2 id="uses-heading">Usage</h2><p>{connection.scope === "personal" ? "Projects where your own runs use this connection." : "Models project runs may use through this connection."}</p></div><span className="fetched-time">{uses.length} uses</span></div>
    <DataTable table={table} label="Model uses" empty="Not used by any project yet." />
    {!canChange ? <p className="card-note">Project admins add and remove this project’s model uses.</p> : null}
    {connection.state === "active" && canChange ? <Disclosure summary="Add a model use" className="card-body" defaultOpen={!uses.length}><AddUse connection={connection} projectId={projectId} /></Disclosure> : null}
    {pending ? <ConfirmDialog busy={remove.isPending} error={error} onClose={() => setPending(null)} onConfirm={() => remove.mutate(pending)} title="Remove model use" confirmLabel="Remove"
      body={<>Stop <strong>{pending.projectName}</strong> from using <strong className="mono">{pending.model}</strong> through this connection? Future attempts lose it; a running actor may still hold a delivered credential until stopped.</>} /> : null}
  </section>;
}
