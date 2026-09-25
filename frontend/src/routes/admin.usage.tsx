import { useMemo } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { useOrg } from "../connection-ui";
import { compact, getUsageOverview, percent, totalTokens, usageOverviewKey, usd } from "../gateway";
import { DashboardLayout } from "../layouts";
import { Card, StatePanel, StatTile } from "../ui";
import { SpendChart } from "../usage-chart";
import { GatewayOff, RangeFilter, SliceTable } from "../usage-ui";

type Search = { days?: number; by?: "project" | "model" };

export const Route = createFileRoute("/admin/usage")({
  component: UsageOverview,
  validateSearch: (search: Record<string, unknown>): Search => ({
    ...([7, 90].includes(Number(search.days)) ? { days: Number(search.days) } : {}),
    ...(search.by === "model" ? { by: "model" as const } : {}),
  }),
});

// Admin → Usage & gateway (docs/model-gateway-plan.md §9.1): stat tiles,
// spend over time by project or model, and the top projects, users and runs.
function UsageOverview() {
  const { org } = useOrg();
  const search = Route.useSearch();
  const navigate = useNavigate({ from: Route.fullPath });
  const days = search.days ?? 30;
  const by = search.by ?? "project";
  const overview = useQuery({ queryKey: usageOverviewKey(org, days, by), enabled: Boolean(org), placeholderData: keepPreviousData,
    queryFn: ({ signal }) => getUsageOverview(days, by, signal) });
  const data = overview.data;
  const points = useMemo(() => (data?.series ?? []).map((p) => ({ day: p.day, key: p.key, cost: Number(p.costUsdMicros) })), [data]);
  const labels = useMemo(() => new Map((data?.seriesLabels ?? []).map((s) => [s.key, s.label])), [data]);
  const set = (next: Search) => void navigate({ search: (prev: Search) => ({ ...prev, ...next }), replace: true });
  const filters = <RangeFilter days={days} onDays={(d) => set({ days: d === 30 ? undefined : d })} />;

  if (overview.isPending) return <DashboardLayout title="Usage & gateway" actions={filters}><StatePanel kind="loading" title="Loading usage" /></DashboardLayout>;
  if (overview.isError || !data) return <DashboardLayout title="Usage & gateway"><StatePanel kind="error" title="Usage is unavailable" retry={() => void overview.refetch()} /></DashboardLayout>;
  if (!data.enabled) return <DashboardLayout title="Usage & gateway"><GatewayOff admin /></DashboardLayout>;
  const t = data.totals!;
  const requests = Number(t.requests);
  return <DashboardLayout title="Usage & gateway" description={`Model traffic through the gateway, ${data.fromDay} to ${data.toDay} (UTC). Counts and metadata only; no prompts or responses are stored.`}
    actions={filters}
    tiles={<>
      <StatTile label="Estimated spend" value={usd(t.costUsdMicros)} meta={`${compact(requests)} requests`} />
      <StatTile label="Tokens" value={compact(totalTokens(t))} meta={`${compact(t.inputTokens)} in · ${compact(t.outputTokens)} out · ${compact(t.cacheReadTokens)} cached`} />
      <StatTile label="Requests" value={compact(requests)} />
      <StatTile label="Error rate" value={percent(Number(t.errors), requests)} tone={requests && Number(t.errors) / requests > 0.05 ? "attention" : undefined} meta={`${compact(t.errors)} failed`} />
      <StatTile label="429 rate" value={percent(Number(t.rateLimited), requests)} tone={requests && Number(t.rateLimited) / requests > 0.02 ? "attention" : undefined} meta={`${compact(t.rateLimited)} rate limited`} />
      <StatTile label="Median time to first token" value={data.medianTtftMs ? `${(Number(data.medianTtftMs) / 1000).toFixed(2)} s` : "—"} />
    </>}>
    <div className={`dash-wide${overview.isPlaceholderData ? " is-refetching" : ""}`}>
      <Card title="Spend over time" description="Estimated daily spend. The top five keep their color; the rest are grouped as Other."
        actions={<span className="segmented" role="group" aria-label="Group spend by">
          <button type="button" aria-pressed={by === "project"} onClick={() => set({ by: undefined })}>By project</button>
          <button type="button" aria-pressed={by === "model"} onClick={() => set({ by: "model" })}>By model</button></span>}>
        {points.length ? <SpendChart points={points} labels={labels} from={data.fromDay} to={data.toDay} title={`Estimated spend by ${by}`} />
          : <p className="card-note">No metered requests in this period.</p>}
      </Card>
    </div>
    <div className={`dash-wide usage-split${overview.isPlaceholderData ? " is-refetching" : ""}`}>
      <Card title="Top projects"><SliceTable id="usage-top-projects" label="Top projects by cost" kind="project" slices={data.topProjects} empty="No project usage in this period." /></Card>
      <Card title="Top users"><SliceTable id="usage-top-users" label="Top users by cost" kind="user" slices={data.topUsers} empty="No user usage in this period." /></Card>
    </div>
    <div className="dash-wide">
      <Card title="Top runs" description="Runs with metered requests in this period, by their total estimated cost."><SliceTable id="usage-top-runs" label="Top runs by cost" kind="run" slices={data.topRuns} empty="No run usage in this period." /></Card>
    </div>
  </DashboardLayout>;
}
