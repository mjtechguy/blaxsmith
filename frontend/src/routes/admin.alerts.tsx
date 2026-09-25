import { useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { BellRing } from "lucide-react";
import { AlertsTable } from "../budget-ui";
import { budgetAlertsKey, budgetsKey, listBudgetAlerts } from "../budgets";
import { useOrg } from "../connection-ui";
import { useGatewayEnabled } from "../gateway";
import { DashboardLayout } from "../layouts";
import { Card, EmptyState, StatTile } from "../ui";
import { GatewayOff } from "../usage-ui";

export const Route = createFileRoute("/admin/alerts")({ component: AlertsPage });

// Admin → Alerts (docs/model-gateway-plan.md §8, §9.1): every budget
// threshold crossing in the organization, with acknowledge and snooze.
function AlertsPage() {
  const { org } = useOrg();
  const gateway = useGatewayEnabled();
  const queryClient = useQueryClient();
  const alerts = useQuery({ queryKey: budgetAlertsKey(org, false), enabled: Boolean(org), refetchInterval: 60_000, queryFn: ({ signal }) => listBudgetAlerts(false, signal) });
  const list = alerts.data?.alerts ?? [];
  const open = list.filter((a) => !a.acknowledgedAt);
  const invalidate = () => Promise.all([queryClient.invalidateQueries({ queryKey: ["gateway-alerts", org] }), queryClient.invalidateQueries({ queryKey: budgetsKey(org) })]);
  if (!gateway && !alerts.data?.alerts.length && !alerts.isPending) return <DashboardLayout title="Alerts"><GatewayOff admin /></DashboardLayout>;
  return <DashboardLayout title="Alerts" description="Budget threshold crossings, newest first. Each threshold fires once per budget and month; acknowledging closes it for every recipient."
    tiles={<>
      <StatTile label="Open" value={open.length} tone={open.length ? "attention" : undefined} />
      <StatTile label="At or over 100%" value={open.filter((a) => a.thresholdPct >= 100).length} tone={open.some((a) => a.thresholdPct >= 100) ? "danger" : undefined} />
      <StatTile label="All alerts" value={list.length} meta="The newest 200" />
    </>}>
    <div className="dash-wide">
      <Card title="Alert feed" description={<>Soft alerts only: nothing is blocked. Manage amounts and thresholds in <Link className="text-link" to="/admin/budgets">Admin → Budgets</Link>.</>}>
        <AlertsTable id="gateway-alerts" alerts={list} loading={alerts.isPending} invalidate={invalidate}
          error={alerts.isError ? <>Alerts are unavailable. <button type="button" className="text-action" onClick={() => void alerts.refetch()}>Try again</button></> : undefined}
          empty={<EmptyState icon={<BellRing size={22} aria-hidden="true" />} title="No alerts">Alerts appear here when spend crosses a budget threshold.</EmptyState>} />
      </Card>
    </div>
  </DashboardLayout>;
}
