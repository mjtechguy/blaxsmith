import { useMemo, useState, type FormEvent } from "react";
import { ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { AlertTriangle } from "lucide-react";
import { useOrg } from "../connection-ui";
import { CollectionTable, useLocalView, type GridColumn } from "../data-table";
import { compact, countdown, familyKinds, familyLabels, gatewayRoutesKey, listGatewayRoutes, routeKindLabels, saveGatewayPool, saveGatewayRoute, setGatewayRouteState, strategyLabels, useNow } from "../gateway";
import type { GatewayPool, GatewayRoute, GatewayRouteMetric, GatewayRouteOption, ListGatewayRoutesResponse, PacedStage } from "../gen/blaxsmith/api/v1/gateway_pb";
import { DashboardLayout } from "../layouts";
import { Card, sentence, StatePanel, StatTile, Timestamp } from "../ui";

export const Route = createFileRoute("/admin/routes")({ component: RoutesAndPools });

// Admin → Routes & pools (docs/model-gateway-plan.md §4, §5, §9.1). Routes are
// organization API-key and cloud connections; personal subscriptions are
// never routes, never pooled, and never failed over to or from.
function RoutesAndPools() {
  const { org } = useOrg();
  const routes = useQuery({ queryKey: gatewayRoutesKey(org), enabled: Boolean(org), refetchInterval: 10_000,
    queryFn: ({ signal }) => listGatewayRoutes(signal) });
  const title = "Routes & pools";
  if (routes.isPending) return <DashboardLayout title={title}><StatePanel kind="loading" title="Loading routes" /></DashboardLayout>;
  if (routes.isError) return <DashboardLayout title={title}><StatePanel kind="error" title="Routes are unavailable" retry={() => void routes.refetch()} /></DashboardLayout>;
  return <RoutesView data={routes.data} org={org} />;
}

export function RoutesView({ data, org }: { data: ListGatewayRoutesResponse; org: string }) {
  const open = data.routes.filter((r) => r.breaker !== "closed").length;
  return <DashboardLayout title="Routes & pools"
    description="API-key and cloud routes your organization owns, grouped into pools for health-based selection and failover before the first byte. Members' own subscriptions are never routes or pool members."
    tiles={<>
      <StatTile label="Routes" value={data.routes.length} meta={`${data.routes.filter((r) => r.state === "enabled").length} enabled`} />
      <StatTile label="Open breakers" value={open} tone={open ? "attention" : undefined} />
      <StatTile label="Pools" value={data.pools.length} />
      <StatTile label="Queued for headroom" value={data.paced.length} tone={data.paced.length ? "attention" : undefined} />
    </>}>
    {!data.poolsEnabled || !data.pacingEnabled ? <div className="dash-wide"><p className="flag-warning" role="note"><AlertTriangle size={14} aria-hidden="true" />
      {!data.poolsEnabled ? "Pools & failover is off: each run uses only its own connection. " : ""}
      {!data.pacingEnabled ? "Rate-aware pacing is off: stages are not queued for headroom. " : ""}
      Turn them on in <Link className="text-link" to="/admin/settings/model-gateway">Settings → Model gateway</Link>.</p></div> : null}
    <div className="dash-wide"><RouteTable routes={data.routes} pools={data.pools} org={org} /></div>
    {data.paced.length ? <div className="dash-wide"><PacedTable paced={data.paced} pools={data.pools} /></div> : null}
    <div className="dash-wide"><Pools data={data} org={org} /></div>
    <div className="dash-wide"><NewRoute connections={data.connections} org={org} /></div>
  </DashboardLayout>;
}

function Countdown({ at, prefix = "resets in " }: { at: string; prefix?: string }) {
  const now = useNow();
  const left = countdown(at, now);
  return left ? <>{prefix}{left}</> : null;
}

function Headroom({ metric }: { metric?: GatewayRouteMetric }) {
  if (!metric || !metric.limit) return <span className="muted">Not reported</span>;
  const limit = Number(metric.limit), remaining = Number(metric.remaining);
  return <span className="headroom-cell">
    <meter min={0} max={limit} value={remaining} low={limit * 0.2} optimum={limit} aria-label={`${remaining} of ${limit} remaining`} />
    <span>{compact(remaining)} / {compact(limit)}</span>
    {metric.resetAt ? <small><Countdown at={metric.resetAt} /></small> : null}
  </span>;
}

const metric = (r: GatewayRoute, name: string) => r.metrics.find((m) => m.name === name);

export function RouteTable({ routes, pools, org }: { routes: GatewayRoute[]; pools: GatewayPool[]; org: string }) {
  const queryClient = useQueryClient();
  const [view, setView] = useLocalView({ size: 25 });
  const [message, setMessage] = useState("");
  const poolName = useMemo(() => new Map(pools.map((p) => [p.id, p.name])), [pools]);
  const action = useMutation({
    mutationFn: ({ id, state }: { id: string; state: string }) => setGatewayRouteState(id, state),
    onSuccess: async (_, { state }) => { setMessage(`Route ${state === "enabled" ? "re-enabled" : state}.`); await queryClient.invalidateQueries({ queryKey: gatewayRoutesKey(org) }); },
    onError: () => setMessage("The route could not be changed. Please try again."),
  });
  const columns = useMemo<GridColumn<GatewayRoute>[]>(() => [
    { id: "name", accessorKey: "name", header: "Route", enableHiding: false, cell: ({ row }) => <span className="task-stage"><strong>{row.original.name}</strong>
      <small>{routeKindLabels[row.original.kind] ?? row.original.kind}{row.original.region ? ` · ${row.original.region}` : ""}{row.original.connectionLabel ? ` · ${row.original.connectionLabel}` : ""}</small></span> },
    { id: "pools", accessorFn: (r) => r.poolIds.map((id) => poolName.get(id) ?? id).join(", "), header: "Pools",
      cell: ({ getValue }) => String(getValue()) || <span className="muted">None</span> },
    { id: "models", accessorFn: (r) => Object.keys(r.modelMap).length, header: "Models",
      cell: ({ row }) => Object.keys(row.original.modelMap).length ? <span title={Object.entries(row.original.modelMap).map(([a, b]) => `${a} → ${b}`).join("\n")}>{Object.keys(row.original.modelMap).length} mapped</span> : <span className="muted">All, unchanged</span> },
    { id: "health", accessorKey: "breaker", header: "Health", cell: ({ row }) => {
      const r = row.original;
      return <span className="task-stage"><span className={`state-badge state-${r.breaker}`}>{r.breaker === "closed" ? "Healthy" : r.breaker === "open" ? "Breaker open" : "Probing"}</span>
        {r.cooldownUntil ? <small><Countdown at={r.cooldownUntil} prefix="cooling down, " /></small> : null}</span>;
    } },
    { id: "requests", accessorFn: (r) => Number(metric(r, "requests")?.remaining ?? -1), header: "Requests left", cell: ({ row }) => <Headroom metric={metric(row.original, "requests")} /> },
    { id: "tokens", accessorFn: (r) => Number((metric(r, "tokens") ?? metric(r, "input-tokens"))?.remaining ?? -1), header: "Tokens left",
      cell: ({ row }) => <Headroom metric={metric(row.original, "tokens") ?? metric(row.original, "input-tokens")} /> },
    { id: "concurrency", accessorKey: "inflight", header: "In flight", cell: ({ row }) => <span className="num">{row.original.inflight}{row.original.concurrencyCap ? ` / ${row.original.concurrencyCap}` : ""}</span> },
    { id: "errors", accessorFn: (r) => r.requests15m ? r.errors15m / r.requests15m : 0, header: "Errors (15 min)",
      cell: ({ row }) => <span className="num">{row.original.errors15m} / {row.original.requests15m}</span> },
    { id: "state", accessorKey: "state", header: "State", cell: ({ row }) => {
      const r = row.original;
      const busy = action.isPending;
      return <span className="task-stage"><span className={`state-badge state-${r.state === "enabled" ? "active" : r.state}`}>{sentence(r.state)}</span>
        <span className="route-actions">
          {r.state !== "draining" ? <button type="button" className="text-action" disabled={busy} onClick={() => action.mutate({ id: r.id, state: "draining" })}>Drain</button> : null}
          {r.state !== "disabled" ? <button type="button" className="text-action text-action-danger" disabled={busy} onClick={() => action.mutate({ id: r.id, state: "disabled" })}>Disable</button> : null}
          {r.state !== "enabled" ? <button type="button" className="text-action" disabled={busy} onClick={() => action.mutate({ id: r.id, state: "enabled" })}>Re-enable</button> : null}
        </span></span>;
    } },
  ], [poolName, action]);
  return <Card title="Routes" description="Live state as the gateway last reported it, refreshed every 10 seconds. Draining routes are used only when no enabled route has headroom; disabled routes are never used.">
    <CollectionTable id="gateway-routes" label="Routes" columns={columns} data={routes} getRowId={(r) => r.id} view={view} onView={setView}
      searchLabel="Search routes" noun="routes" empty="No routes yet. Add one below from an organization API key or a cloud account."
      renderExpanded={(r) => <dl className="route-detail">
        {r.metrics.map((m) => <div key={m.name}><dt>{sentence(m.name)}</dt><dd>{m.remaining.toString()} of {m.limit.toString()} left{m.resetAt ? <> · <Countdown at={m.resetAt} /></> : null}</dd></div>)}
        <div><dt>Last 429</dt><dd>{r.last429At ? <Timestamp value={r.last429At} /> : "None recorded"}</dd></div>
        <div><dt>Priority · weight</dt><dd>{r.priority} · {r.weight}</dd></div>
        <div><dt>Configured quota</dt><dd>{r.requestsPerMinute || r.tokensPerMinute ? `${r.requestsPerMinute || "—"} req/min · ${r.tokensPerMinute ? compact(r.tokensPerMinute) : "—"} tokens/min` : "None"}</dd></div>
        {Object.entries(r.modelMap).map(([ours, theirs]) => <div key={ours}><dt className="mono">{ours}</dt><dd className="mono">{theirs}</dd></div>)}
        <div><dt>State reported</dt><dd>{r.stateUpdatedAt ? <Timestamp value={r.stateUpdatedAt} /> : "No traffic yet"}</dd></div>
      </dl>} expandLabel={(r) => r.name} />
    {message ? <p className="card-note" role="status">{message}</p> : null}
  </Card>;
}

function PacedTable({ paced, pools }: { paced: PacedStage[]; pools: GatewayPool[] }) {
  const [view, setView] = useLocalView({ size: 10 });
  const poolName = useMemo(() => new Map(pools.map((p) => [p.id, p.name])), [pools]);
  const columns = useMemo<GridColumn<PacedStage>[]>(() => [
    { id: "stage", accessorKey: "stage", header: "Stage", enableHiding: false, cell: ({ row }) => <Link className="row-title" to="/projects/$projectId/runs/$runId"
      params={{ projectId: row.original.projectId, runId: row.original.runId }}>{row.original.stage}</Link> },
    { id: "pool", accessorFn: (p) => poolName.get(p.poolId) ?? p.poolId, header: "Pool" },
    { id: "reason", accessorKey: "reason", header: "Waiting for", cell: ({ row }) => <span className="task-stage"><strong>{row.original.reason}</strong>
      <small>{row.original.resetsAt ? <Countdown at={row.original.resetsAt} /> : "Reset time not reported"}</small></span> },
    { id: "since", accessorKey: "since", header: "Queued since", cell: ({ row }) => <Timestamp value={row.original.since} /> },
  ], [poolName]);
  return <Card title="Queued for headroom" description="Ready stages the dispatcher is holding until their pool has headroom, instead of failing them.">
    <CollectionTable id="gateway-paced" label="Stages queued for headroom" columns={columns} data={paced} getRowId={(p) => p.taskId} view={view} onView={setView} noun="stages" empty="Nothing is queued." />
  </Card>;
}

function errorText(cause: unknown, fallback: string) {
  return cause instanceof ConnectError ? `${fallback} ${cause.rawMessage}` : cause instanceof Error ? cause.message : fallback;
}

function Pools({ data, org }: { data: ListGatewayRoutesResponse; org: string }) {
  return <Card title="Pools" description="A pool serves one model family. Selection skips unhealthy, cooling-down, saturated and unsupported routes, then applies the strategy. A pool applies to a run when it contains that run's connection and is granted to its project.">
    <div className="pool-list">
      {data.pools.map((pool) => <details key={pool.id} className="pool-item">
        <summary>{pool.name} <small>{familyLabels[pool.family] ?? pool.family} · {strategyLabels[pool.strategy] ?? pool.strategy} · {pool.routeIds.length} routes · {pool.projectIds.length} projects</small>
          {pool.state === "disabled" ? <span className="state-badge">Disabled</span> : null}
          <Link className="text-action" to="/admin/pools/$poolId" params={{ poolId: pool.id }}>Traffic & failovers</Link></summary>
        <PoolForm pool={pool} data={data} org={org} />
      </details>)}
      <details className="pool-item" open={!data.pools.length}>
        <summary>New pool</summary>
        <PoolForm data={data} org={org} />
      </details>
    </div>
  </Card>;
}

function toggle(list: string[], id: string, on: boolean) {
  return on ? [...new Set([...list, id])] : list.filter((x) => x !== id);
}

export function PoolForm({ pool, data, org }: { pool?: GatewayPool; data: ListGatewayRoutesResponse; org: string }) {
  const queryClient = useQueryClient();
  const [draft, setDraft] = useState({ name: pool?.name ?? "", family: pool?.family ?? "anthropic", strategy: pool?.strategy ?? "priority_headroom",
    concurrencyCap: String(pool?.concurrencyCap ?? 0), affinity: pool?.affinity ?? true, state: pool?.state ?? "enabled",
    routeIds: pool?.routeIds ?? [], projectIds: pool?.projectIds ?? [] });
  const [message, setMessage] = useState("");
  const eligible = data.routes.filter((r) => familyKinds[draft.family]?.includes(r.kind));
  const save = useMutation({
    mutationFn: () => saveGatewayPool({ id: pool?.id ?? "", name: draft.name.trim(), family: draft.family, strategy: draft.strategy,
      concurrencyCap: Number(draft.concurrencyCap) || 0, affinity: draft.affinity, state: draft.state,
      routeIds: draft.routeIds.filter((id) => eligible.some((r) => r.id === id)), projectIds: draft.projectIds }),
    onSuccess: async () => { setMessage("Pool saved."); if (!pool) setDraft({ ...draft, name: "", routeIds: [], projectIds: [] }); await queryClient.invalidateQueries({ queryKey: gatewayRoutesKey(org) }); },
    onError: (cause) => setMessage(errorText(cause, "The pool could not be saved.")),
  });
  const submit = (event: FormEvent) => { event.preventDefault(); setMessage(""); save.mutate(); };
  const id = pool?.id ?? "new";
  return <form className="price-form" noValidate onSubmit={submit} aria-label={pool ? `Edit pool ${pool.name}` : "Create a pool"}>
    <label className="form-field"><span>Name</span><input value={draft.name} required maxLength={64} onChange={(e) => setDraft({ ...draft, name: e.target.value })} placeholder="Claude production" /></label>
    <label className="form-field"><span>Model family</span><select value={draft.family} disabled={Boolean(pool)} onChange={(e) => setDraft({ ...draft, family: e.target.value, routeIds: [] })}>
      {Object.entries(familyLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
    <label className="form-field"><span>Strategy</span><select value={draft.strategy} onChange={(e) => setDraft({ ...draft, strategy: e.target.value })}>
      {Object.entries(strategyLabels).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>
    <label className="form-field"><span>Pool concurrency cap (0 = none)</span><input inputMode="numeric" value={draft.concurrencyCap} onChange={(e) => setDraft({ ...draft, concurrencyCap: e.target.value })} /></label>
    <label className="form-field"><span>State</span><select value={draft.state} onChange={(e) => setDraft({ ...draft, state: e.target.value })}>
      <option value="enabled">Enabled</option><option value="disabled">Disabled</option></select></label>
    <label className="form-field"><span>Prompt-cache affinity</span><select value={draft.affinity ? "on" : "off"} onChange={(e) => setDraft({ ...draft, affinity: e.target.value === "on" })}>
      <option value="on">Prefer the attempt's last route</option><option value="off">Off</option></select></label>
    <fieldset className="check-grid form-wide"><legend>Routes</legend>
      {eligible.length ? eligible.map((r) => <label key={r.id}><input type="checkbox" checked={draft.routeIds.includes(r.id)}
        onChange={(e) => setDraft({ ...draft, routeIds: toggle(draft.routeIds, r.id, e.target.checked) })} id={`${id}-route-${r.id}`} />{r.name} <span className="muted">({routeKindLabels[r.kind] ?? r.kind})</span></label>)
        : <span className="muted">No routes serve this family yet.</span>}</fieldset>
    <fieldset className="check-grid form-wide"><legend>Granted to projects</legend>
      {data.projects.length ? data.projects.map((p: GatewayRouteOption) => <label key={p.id}><input type="checkbox" checked={draft.projectIds.includes(p.id)}
        onChange={(e) => setDraft({ ...draft, projectIds: toggle(draft.projectIds, p.id, e.target.checked) })} />{p.label}</label>)
        : <span className="muted">No projects yet.</span>}</fieldset>
    <button type="submit" className="secondary-button" disabled={save.isPending || !draft.name.trim()}>{save.isPending ? "Saving…" : pool ? "Save pool" : "Create pool"}</button>
    {message ? <p className="card-note form-wide" role="status">{message}</p> : null}
  </form>;
}

// "ours=theirs" per line → model map.
export function parseModelMap(text: string): Record<string, string> | null {
  const out: Record<string, string> = {};
  for (const line of text.split("\n").map((l) => l.trim()).filter(Boolean)) {
    const [ours, theirs, ...rest] = line.split("=").map((s) => s.trim());
    if (!ours || !theirs || rest.length) return null;
    out[ours] = theirs;
  }
  return out;
}

const apiKinds: Record<string, string> = { anthropic: "anthropic", openai: "openai", opencode_zen: "opencode", opencode_go: "opencode-go" };

function NewRoute({ connections, org }: { connections: GatewayRouteOption[]; org: string }) {
  const queryClient = useQueryClient();
  const empty = { kind: "anthropic", name: "", connectionId: "", region: "", cloudProject: "", azureResource: "", apiVersion: "2024-10-21", models: "", priority: "100", weight: "1", cap: "0", rpm: "0", tpm: "0", credential: "" };
  const [draft, setDraft] = useState(empty);
  const [message, setMessage] = useState("");
  const regional = draft.kind === "bedrock" || draft.kind === "vertex";
  const azure = draft.kind === "azure_openai";
  const cloud = regional || azure;
  const keys = connections.filter((c) => c.detail === apiKinds[draft.kind]);
  const save = useMutation({
    mutationFn: () => {
      const modelMap = parseModelMap(draft.models);
      if (!modelMap) throw new Error("Write one model mapping per line as our-model-id=route-model-id.");
      if (cloud && !Object.keys(modelMap).length) throw new Error(azure ? "Map each model to its Azure deployment name." : "Cloud routes need at least one model mapping.");
      return saveGatewayRoute({ name: draft.name.trim(), kind: draft.kind, connectionId: cloud ? "" : draft.connectionId, region: regional ? draft.region.trim() : "",
        azureResource: azure ? draft.azureResource.trim() : "", apiVersion: azure ? draft.apiVersion.trim() : "",
        cloudProject: draft.kind === "vertex" ? draft.cloudProject.trim() : "", modelMap, priority: Number(draft.priority) || 0, weight: Number(draft.weight) || 1,
        concurrencyCap: Number(draft.cap) || 0, requestsPerMinute: Number(draft.rpm) || 0, tokensPerMinute: BigInt(Number(draft.tpm) || 0) }, cloud ? draft.credential.trim() : "");
    },
    // The credential is cleared from memory as soon as it is sent.
    onSuccess: async () => { setDraft(empty); setMessage("Route added. It serves traffic once a pool that contains it is granted to a project."); await queryClient.invalidateQueries({ queryKey: gatewayRoutesKey(org) }); },
    onError: (cause) => { setDraft({ ...draft, credential: "" }); setMessage(errorText(cause, "The route could not be added.")); },
  });
  const field = (key: keyof typeof draft, label: string, placeholder = "") => <label className="form-field"><span>{label}</span>
    <input value={draft[key]} placeholder={placeholder} onChange={(e) => setDraft({ ...draft, [key]: e.target.value })} /></label>;
  return <Card title="Add a route" description="Routes are organization-owned API keys, or cloud accounts your organization owns: Amazon Bedrock (an AWS access key), Google Vertex AI (a service-account key) and Azure OpenAI (a resource key). Cloud credentials are encrypted and never shown again.">
    <form className="price-form" noValidate onSubmit={(e) => { e.preventDefault(); setMessage(""); save.mutate(); }} aria-label="Add a route">
      <label className="form-field"><span>Kind</span><select value={draft.kind} onChange={(e) => setDraft({ ...draft, kind: e.target.value, connectionId: "" })}>
        {["anthropic", "bedrock", "vertex", "openai", "azure_openai", "opencode_zen", "opencode_go"].map((k) => <option key={k} value={k}>{routeKindLabels[k]}</option>)}</select></label>
      {field("name", "Name", draft.kind === "bedrock" ? "bedrock-us-east-1" : "anthropic-key-a")}
      {regional ? <>
        {field("region", "Region", draft.kind === "bedrock" ? "us-east-1" : "us-east5")}
        {draft.kind === "vertex" ? field("cloudProject", "Google Cloud project", "my-project") : null}
      </> : azure ? <>
        {field("azureResource", "Azure resource name", "contoso-ai")}
        {field("apiVersion", "API version", "2024-10-21")}
      </> : <label className="form-field"><span>Organization API key</span><select value={draft.connectionId} onChange={(e) => setDraft({ ...draft, connectionId: e.target.value })}>
        <option value="">Choose a connection</option>{keys.map((c) => <option key={c.id} value={c.id}>{c.label}</option>)}</select></label>}
      {field("priority", "Priority (lower first)")}
      {field("weight", "Weight")}
      {field("cap", "Concurrency cap (0 = none)")}
      {regional ? <>{field("rpm", "Quota: requests/min")}{field("tpm", "Quota: tokens/min")}</> : null}
      <label className="form-field form-wide"><span>Model mapping{cloud ? "" : " (optional)"}</span>
        <textarea value={draft.models} spellCheck={false} placeholder={draft.kind === "bedrock" ? "claude-opus-5-5=us.anthropic.claude-opus-5-5-v1:0" : draft.kind === "vertex" ? "claude-opus-5-5=claude-opus-5-5@20260901" : azure ? "gpt-6-luna=my-luna-deployment" : "Leave empty to pass model ids through unchanged"}
          onChange={(e) => setDraft({ ...draft, models: e.target.value })} /></label>
      {cloud ? <label className="form-field form-wide"><span>{draft.kind === "bedrock" ? "AWS access key (JSON)" : azure ? "Azure OpenAI key" : "Service-account key (JSON)"}</span>
        <textarea value={draft.credential} spellCheck={false} autoComplete="off" placeholder={draft.kind === "bedrock" ? '{"access_key_id": "…", "secret_access_key": "…"}' : azure ? "Key 1 or key 2 from the Azure portal" : '{"type": "service_account", "client_email": "…", "private_key": "…"}'}
          onChange={(e) => setDraft({ ...draft, credential: e.target.value })} /></label> : null}
      <button type="submit" className="secondary-button" disabled={save.isPending || !draft.name.trim() || (!cloud && !draft.connectionId) || (cloud && !draft.credential.trim())}>
        {save.isPending ? "Adding…" : "Add route"}</button>
      {message ? <p className="card-note form-wide" role="status">{message}</p> : null}
    </form>
  </Card>;
}
