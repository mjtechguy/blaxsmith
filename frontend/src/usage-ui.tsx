// Shared pieces of the usage pages: the date-range filter, slice tables, and
// the "gateway off" state. All costs are estimates.
import { useMemo, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { Waypoints } from "lucide-react";
import { CollectionTable, useLocalView, type GridColumn } from "./data-table";
import { compact, totalTokens, usd } from "./gateway";
import type { UsageSlice } from "./gen/blaxsmith/api/v1/gateway_pb";
import { EmptyState } from "./ui";

export const ranges = [{ days: 7, label: "Last 7 days" }, { days: 30, label: "Last 30 days" }, { days: 90, label: "Last 90 days" }];

export function RangeFilter({ days, onDays, children }: { days: number; onDays: (days: number) => void; children?: ReactNode }) {
  return <div className="filter-row" role="group" aria-label="Filters">
    <span className="segmented" role="group" aria-label="Date range">{ranges.map((r) => <button key={r.days} type="button" aria-pressed={days === r.days} onClick={() => onDays(r.days)}>{r.label}</button>)}</span>
    {children}
    <span className="estimate-badge" title="Costs use list or contracted rates, not the provider's bill.">Estimated costs</span>
  </div>;
}

export function GatewayOff({ admin }: { admin?: boolean }) {
  return <EmptyState icon={<Waypoints size={22} aria-hidden="true" />} title="The model gateway is off">
    Usage and cost appear here once an administrator turns on the model gateway{admin ? <> in <Link className="text-link" to="/admin/settings/model-gateway">Settings → Model gateway</Link></> : ""}. Runs that receive provider keys directly are not metered.
  </EmptyState>;
}

type Kind = "project" | "user" | "model" | "run" | "stage";

export function SliceTable({ id, label, kind, slices, loading, error, empty }: {
  id: string; label: string; kind: Kind; slices: UsageSlice[]; loading?: boolean; error?: ReactNode; empty: string;
}) {
  const [view, setView] = useLocalView({ size: 10, sort: [{ id: "cost", desc: true }] });
  const columns = useMemo<GridColumn<UsageSlice>[]>(() => [
    { id: "name", accessorKey: "label", header: kind === "run" ? "Run" : kind === "user" ? "User" : kind === "model" ? "Model" : kind === "stage" ? "Stage" : "Project", enableHiding: false,
      cell: ({ row }) => {
        const s = row.original;
        if (kind === "run") return <span className="task-stage"><Link className="row-title" to="/projects/$projectId/runs/$runId" params={{ projectId: s.projectId, runId: s.key }} search={{ tab: "cost" }}>{s.label}</Link><small>{s.detail}</small></span>;
        if (kind === "stage") return <strong>{s.label}</strong>;
        if (kind === "model") return <span className="task-stage"><strong className="mono">{s.label}</strong><small>{s.detail}</small></span>;
        if (kind === "project") return <Link className="row-title" to="/projects/$projectId" params={{ projectId: s.key }}>{s.label}</Link>;
        return <span className="task-stage"><strong>{s.label}</strong>{s.detail ? <small>@{s.detail}</small> : null}</span>;
      } },
    { id: "cost", accessorFn: (s) => Number(s.totals?.costUsdMicros ?? 0), header: "Est. cost", cell: ({ row }) => <strong className="num">{usd(row.original.totals?.costUsdMicros ?? 0n)}</strong> },
    { id: "tokens", accessorFn: (s) => totalTokens(s.totals), header: "Tokens", cell: ({ row }) => <span className="num" title={`${row.original.totals?.inputTokens} in · ${row.original.totals?.outputTokens} out · ${row.original.totals?.cacheReadTokens} cache read · ${row.original.totals?.cacheWriteTokens} cache write`}>{compact(totalTokens(row.original.totals))}</span> },
    { id: "requests", accessorFn: (s) => Number(s.totals?.requests ?? 0), header: "Requests", cell: ({ row }) => <span className="num">{compact(row.original.totals?.requests ?? 0n)}</span> },
    { id: "errors", accessorFn: (s) => Number(s.totals?.errors ?? 0), header: "Errors", cell: ({ row }) => <span className="num">{compact(row.original.totals?.errors ?? 0n)}</span> },
  ], [kind]);
  return <CollectionTable id={id} label={label} columns={columns} data={slices} getRowId={(s) => s.key} view={view} onView={setView}
    loading={loading} error={error} empty={empty} noun={kind === "run" ? "runs" : `${kind}s`} defaultHidden={["errors"]} />;
}
