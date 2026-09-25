import { useMemo } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { useOrg } from "../connection-ui";
import { CollectionTable, useLocalView, type GridColumn } from "../data-table";
import { compact, familyLabels, gatewayRoutesKey, getGatewayPoolDetail, listGatewayRoutes, percent, poolDetailKey, routeKindLabels, strategyLabels, usd } from "../gateway";
import type { GatewayFailover, GatewayRouteTraffic, GetGatewayPoolDetailResponse, ListGatewayRoutesResponse } from "../gen/blaxsmith/api/v1/gateway_pb";
import { DetailLayout } from "../layouts";
import { Card, StatePanel, tabFrom, Timestamp, type TabSpec } from "../ui";
import { PoolForm, RouteTable } from "./admin.routes";

type Search = { tab?: string; hours?: number };

const tabs: TabSpec[] = [{ id: "routes", label: "Routes" }, { id: "traffic", label: "Traffic" }, { id: "failovers", label: "Failovers" }, { id: "settings", label: "Settings" }];
const windows = [{ hours: 1, label: "1 hour" }, { hours: 24, label: "24 hours" }, { hours: 168, label: "7 days" }];

export const Route = createFileRoute("/admin/pools/$poolId")({
  component: PoolDetail,
  validateSearch: (search: Record<string, unknown>): Search => ({
    ...(typeof search.tab === "string" ? { tab: search.tab } : {}),
    ...([1, 168].includes(Number(search.hours)) ? { hours: Number(search.hours) } : {}),
  }),
});

// Admin → Routes & pools → a pool (docs/model-gateway-plan.md §9.1): its
// routes, traffic per route and failovers, from the per-attempt usage events.
function PoolDetail() {
  const { org } = useOrg();
  const { poolId } = Route.useParams();
  const search = Route.useSearch();
  const hours = search.hours ?? 24;
  const current = tabFrom(search, tabs);
  const detail = useQuery({ queryKey: poolDetailKey(org, poolId, hours), enabled: Boolean(org), placeholderData: keepPreviousData,
    queryFn: ({ signal }) => getGatewayPoolDetail(poolId, hours, signal) });
  const routes = useQuery({ queryKey: gatewayRoutesKey(org), enabled: Boolean(org), queryFn: ({ signal }) => listGatewayRoutes(signal) });
  const back = { href: "/admin/routes", label: "Routes & pools" };
  if (detail.isPending || routes.isPending) return <DetailLayout back={back} title="Pool"><StatePanel kind="loading" title="Loading pool" /></DetailLayout>;
  if (detail.isError || routes.isError) return <DetailLayout back={back} title="Pool"><StatePanel kind="error" title="This pool is unavailable" retry={() => { void detail.refetch(); void routes.refetch(); }} /></DetailLayout>;
  return <PoolView detail={detail.data} routes={routes.data} org={org} current={current} hours={hours} />;
}

export function PoolView({ detail, routes, org, current, hours }: { detail: GetGatewayPoolDetailResponse; routes: ListGatewayRoutesResponse; org: string; current: string; hours: number }) {
  const navigate = useNavigate();
  const pool = detail.pool!;
  const members = routes.routes.filter((r) => pool.routeIds.includes(r.id));
  const failovers = detail.failovers.length;
  const range = <span className="segmented" role="group" aria-label="Time window">{windows.map((w) => <button key={w.hours} type="button" aria-pressed={hours === w.hours}
    onClick={() => void navigate({ to: "/admin/pools/$poolId", params: { poolId: pool.id }, search: (prev: Search) => ({ ...prev, hours: w.hours === 24 ? undefined : w.hours }) as never, replace: true })}>{w.label}</button>)}</span>;
  return <DetailLayout back={{ href: "/admin/routes", label: "Routes & pools" }} title={pool.name}
    status={<span className={`state-badge state-${pool.state === "enabled" ? "active" : "draining"}`}>{pool.state === "enabled" ? "Enabled" : "Disabled"}</span>}
    facts={[{ label: "Family", value: familyLabels[pool.family] ?? pool.family }, { label: "Strategy", value: strategyLabels[pool.strategy] ?? pool.strategy },
      { label: "Concurrency cap", value: pool.concurrencyCap ? String(pool.concurrencyCap) : "None" }, { label: "Projects", value: String(pool.projectIds.length) }]}
    tabs={tabs.map((t) => t.id === "failovers" ? { ...t, count: failovers } : t)} current={current} tabsLabel="Pool sections"
    actions={current === "traffic" || current === "failovers" ? range : undefined}>
    {current === "routes" ? <RouteTable routes={members} pools={[pool]} org={org} /> : null}
    {current === "traffic" ? <Traffic traffic={detail.traffic} hours={detail.hours} /> : null}
    {current === "failovers" ? <Failovers failovers={detail.failovers} hours={detail.hours} /> : null}
    {current === "settings" ? <Card title="Settings" description="Strategy, caps, prompt-cache affinity, member routes and project grants."><PoolForm pool={pool} data={routes} org={org} /></Card> : null}
  </DetailLayout>;
}

function Traffic({ traffic, hours }: { traffic: GatewayRouteTraffic[]; hours: number }) {
  const [view, setView] = useLocalView({ size: 25 });
  const columns = useMemo<GridColumn<GatewayRouteTraffic>[]>(() => [
    { id: "route", accessorKey: "routeName", header: "Route", enableHiding: false, cell: ({ row }) => <span className="task-stage"><strong>{row.original.routeName}</strong><small>{routeKindLabels[row.original.routeKind] ?? row.original.routeKind}</small></span> },
    { id: "requests", accessorFn: (t) => Number(t.requests), header: "Requests", cell: ({ row }) => <span className="num">{compact(row.original.requests)}</span> },
    { id: "errors", accessorFn: (t) => Number(t.errors), header: "Errors", cell: ({ row }) => <span className="num">{compact(row.original.errors)} ({percent(Number(row.original.errors), Number(row.original.requests))})</span> },
    { id: "rateLimited", accessorFn: (t) => Number(t.rateLimited), header: "429s", cell: ({ row }) => <span className="num">{compact(row.original.rateLimited)}</span> },
    { id: "failovers", accessorFn: (t) => Number(t.failoversFrom), header: "Failed over", cell: ({ row }) => <span className="num">{compact(row.original.failoversFrom)}</span> },
    { id: "tokens", accessorFn: (t) => Number(t.tokens), header: "Tokens", cell: ({ row }) => <span className="num">{compact(row.original.tokens)}</span> },
    { id: "cost", accessorFn: (t) => Number(t.costUsdMicros), header: "Est. cost", cell: ({ row }) => <span className="num">{usd(row.original.costUsdMicros)}</span> },
    { id: "ttft", accessorFn: (t) => Number(t.avgTtftMs), header: "Avg. first token", cell: ({ row }) => <span className="num">{row.original.avgTtftMs ? `${(Number(row.original.avgTtftMs) / 1000).toFixed(2)} s` : "—"}</span> },
  ], []);
  return <Card title="Traffic" description={`Every upstream request through this pool in the last ${hours === 168 ? "7 days" : hours === 1 ? "hour" : "24 hours"}, per route. Failed-over requests count on the route that failed them.`}>
    <CollectionTable id="gateway-pool-traffic" label="Traffic per route" columns={columns} data={traffic} getRowId={(t) => t.routeId} view={view} onView={setView} noun="routes" empty="This pool has no routes." />
  </Card>;
}

function Failovers({ failovers, hours }: { failovers: GatewayFailover[]; hours: number }) {
  const [view, setView] = useLocalView({ size: 25 });
  const columns = useMemo<GridColumn<GatewayFailover>[]>(() => [
    { id: "at", accessorKey: "at", header: "When", enableHiding: false, cell: ({ row }) => <Timestamp value={row.original.at} /> },
    { id: "stage", accessorKey: "stage", header: "Stage", cell: ({ row }) => <Link className="row-title" to="/projects/$projectId/runs/$runId"
      params={{ projectId: row.original.projectId, runId: row.original.runId }}>{row.original.stage}</Link> },
    { id: "from", accessorKey: "fromRouteName", header: "Failed on", cell: ({ row }) => <span className="task-stage"><strong>{row.original.fromRouteName}</strong>
      <small>{row.original.httpStatus === 429 ? "429 rate limited" : row.original.httpStatus === 502 ? "Unreachable" : `HTTP ${row.original.httpStatus}`}</small></span> },
    { id: "to", accessorKey: "toRouteName", header: "Retried on", cell: ({ row }) => <span className="task-stage"><strong>{row.original.toRouteName}</strong>
      <small>{row.original.finalStatus === "ok" ? "Served" : `${row.original.finalStatus} (HTTP ${row.original.finalHttpStatus})`}</small></span> },
  ], []);
  return <Card title="Failovers" description={`Requests that failed on a route before their first byte and were retried on the next, in the last ${hours === 168 ? "7 days" : hours === 1 ? "hour" : "24 hours"} (newest first, at most 200).`}>
    <CollectionTable id="gateway-pool-failovers" label="Failovers" columns={columns} data={failovers} getRowId={(f) => `${f.at}-${f.fromRouteId}-${f.runId}-${f.stage}`} view={view} onView={setView} noun="failovers" empty="No failovers in this window." />
  </Card>;
}
