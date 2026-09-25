import { useMemo } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute } from "@tanstack/react-router";
import { AlertsTable, BudgetMeter } from "../budget-ui";
import { getProjectUsage, projectUsageKey } from "../budgets";
import { compact, totalTokens, usd } from "../gateway";
import { DashboardLayout } from "../layouts";
import { Card, StatePanel, StatTile } from "../ui";
import { SpendChart } from "../usage-chart";
import { GatewayOff, SliceTable } from "../usage-ui";
import { useScope } from "../workspace-ui";

export const Route = createFileRoute("/projects/$projectId/usage")({ component: ProjectUsage });

// Project → Usage (docs/model-gateway-plan.md §9.3): this month's spend
// against the project budget, by stage, model and user, and the most
// expensive runs. Spend by user is shown to project administrators only.
function ProjectUsage() {
  const { projectId } = Route.useParams();
  const { org } = useScope();
  const queryClient = useQueryClient();
  const usage = useQuery({ queryKey: projectUsageKey(org, projectId), enabled: Boolean(org), refetchInterval: 60_000,
    queryFn: ({ signal }) => getProjectUsage(projectId, signal) });
  const data = usage.data;
  const points = useMemo(() => (data?.series ?? []).map((p) => ({ day: p.day, key: p.key, cost: Number(p.costUsdMicros) })), [data]);
  const labels = useMemo(() => new Map((data?.series ?? []).map((p) => [p.key, p.key])), [data]);
  const title = "Usage";
  if (usage.isPending) return <DashboardLayout title={title}><StatePanel kind="loading" title="Loading project usage" /></DashboardLayout>;
  if (usage.isError || !data) return <DashboardLayout title={title}><StatePanel kind="error" title="Project usage is unavailable" retry={() => void usage.refetch()} /></DashboardLayout>;
  if (!data.enabled) return <DashboardLayout title={title}><GatewayOff /></DashboardLayout>;
  const t = data.totals!;
  const b = data.budget;
  return <DashboardLayout title={title} description={`Model usage for this project, ${data.fromDay} to ${data.toDay} (this month, UTC). Costs are estimates; no prompts or responses are stored.`}
    actions={<span className="estimate-badge" title="Costs use list or contracted rates, not the provider's bill.">Estimated costs</span>}
    tiles={<>
      <StatTile label="Spend this month" value={usd(t.costUsdMicros)} meta={b ? `of ${usd(b.amountUsdMicros)} budget` : "No project budget"} />
      <StatTile label="Forecast" value={b ? usd(b.forecastUsdMicros) : "—"} meta="At this month's run-rate" tone={b && Number(b.forecastUsdMicros) > Number(b.amountUsdMicros) ? "attention" : undefined} />
      <StatTile label="Tokens" value={compact(totalTokens(t))} meta={`${compact(t.inputTokens)} in · ${compact(t.outputTokens)} out`} />
      <StatTile label="Requests" value={compact(t.requests)} meta={`${compact(t.errors)} failed`} tone={Number(t.errors) ? "attention" : undefined} />
    </>}>
    <div className="dash-wide">
      <Card title="Budget" description={b ? `${b.name}: alerts at ${b.thresholds.map((x) => `${x}%`).join(", ")}. Budgets never block runs.` : "This project has no budget. Organization owners and admins add one in Admin → Budgets."}>
        {b ? <div className="budget-switch"><BudgetMeter budget={b} /></div> : null}
      </Card>
    </div>
    <div className="dash-wide">
      <Card title="Spend by model" description="Estimated daily spend this month.">
        {points.length ? <SpendChart points={points} labels={labels} from={data.fromDay} to={data.toDay} title="Estimated spend by model" />
          : <p className="card-note">No metered requests this month.</p>}
      </Card>
    </div>
    <div className="dash-wide usage-split">
      <Card title="By stage"><SliceTable id="project-usage-stages" label="Usage by stage" kind="stage" slices={data.byStage} empty="No usage this month." /></Card>
      <Card title="By model"><SliceTable id="project-usage-models" label="Usage by model" kind="model" slices={data.byModel} empty="No usage this month." /></Card>
    </div>
    <div className="dash-wide usage-split">
      <Card title="By user" description={data.byUserHidden ? "Shown to project administrators." : undefined}>
        {data.byUserHidden ? <p className="card-note">Spend by user is visible to this project's administrators and organization admins.</p>
          : <SliceTable id="project-usage-users" label="Usage by user" kind="user" slices={data.byUser} empty="No usage this month." />}
      </Card>
      <Card title="Most expensive runs"><SliceTable id="project-usage-runs" label="Runs by cost" kind="run" slices={data.topRuns} empty="No runs used the gateway this month." /></Card>
    </div>
    {data.alerts.length ? <div className="dash-wide">
      <Card title="Budget alerts" description="Threshold crossings for this project's budget.">
        <AlertsTable id="project-usage-alerts" alerts={data.alerts} showTarget={false} canAct={!data.byUserHidden} empty="No alerts."
          invalidate={() => queryClient.invalidateQueries({ queryKey: projectUsageKey(org, projectId) })} />
      </Card>
    </div> : null}
  </DashboardLayout>;
}
