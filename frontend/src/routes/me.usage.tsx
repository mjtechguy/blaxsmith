import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { createFileRoute, useNavigate } from "@tanstack/react-router";
import { currentSession, sessionQueryKey } from "../auth";
import { compact, getMyUsage, myUsageKey, totalTokens, usd } from "../gateway";
import { DashboardLayout } from "../layouts";
import { Card, StatePanel, StatTile } from "../ui";
import { GatewayOff, RangeFilter, SliceTable } from "../usage-ui";

type Search = { days?: number };

export const Route = createFileRoute("/me/usage")({
  component: MyUsage,
  validateSearch: (search: Record<string, unknown>): Search => ([7, 90].includes(Number(search.days)) ? { days: Number(search.days) } : {}),
});

// Account menu → My usage (docs/model-gateway-plan.md §9.2): the runs you
// started, by project and model. Only your own usage is ever shown here.
function MyUsage() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const scope = session.data ? `${session.data.organizationId}:${session.data.principalId}` : "";
  const days = Route.useSearch().days ?? 30;
  const navigate = useNavigate({ from: Route.fullPath });
  const usage = useQuery({ queryKey: myUsageKey(scope, days), enabled: Boolean(scope), placeholderData: keepPreviousData,
    queryFn: ({ signal }) => getMyUsage(days, signal) });
  const filters = <RangeFilter days={days} onDays={(d) => void navigate({ search: d === 30 ? {} : { days: d }, replace: true })} />;
  const title = "My usage";
  if (usage.isPending) return <DashboardLayout title={title} actions={filters}><StatePanel kind="loading" title="Loading your usage" /></DashboardLayout>;
  if (usage.isError || !usage.data) return <DashboardLayout title={title}><StatePanel kind="error" title="Your usage is unavailable" retry={() => void usage.refetch()} /></DashboardLayout>;
  const u = usage.data;
  if (!u.enabled) return <DashboardLayout title={title}><GatewayOff /></DashboardLayout>;
  const t = u.totals!;
  const fading = usage.isPlaceholderData ? " is-refetching" : "";
  return <DashboardLayout title={title} description={`Model usage from runs you started, ${u.fromDay} to ${u.toDay} (UTC).`} actions={filters}
    tiles={<>
      <StatTile label="My estimated spend" value={usd(t.costUsdMicros)} meta={`${compact(t.requests)} requests`} />
      <StatTile label="My tokens" value={compact(totalTokens(t))} meta={`${compact(t.inputTokens)} in · ${compact(t.outputTokens)} out`} />
      <StatTile label="Cached input" value={compact(t.cacheReadTokens)} meta="Prompt-cache reads" />
      <StatTile label="Projects" value={u.byProject.length} />
    </>}>
    <div className={`dash-wide usage-split${fading}`}>
      <Card title="By project"><SliceTable id="my-usage-projects" label="My usage by project" kind="project" slices={u.byProject} empty="No usage in this period." /></Card>
      <Card title="By model"><SliceTable id="my-usage-models" label="My usage by model" kind="model" slices={u.byModel} empty="No usage in this period." /></Card>
    </div>
    <div className={`dash-wide${fading}`}>
      <Card title="My runs by cost" description="Your runs with metered requests in this period."><SliceTable id="my-usage-runs" label="My runs by cost" kind="run" slices={u.topRuns} empty="None of your runs used the gateway in this period." /></Card>
    </div>
  </DashboardLayout>;
}
