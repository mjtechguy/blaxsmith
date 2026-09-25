// Shared pieces of the budget pages: the progress meter with its forecast,
// and the alert feed with acknowledge and snooze. All amounts are estimates.
import { useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { failure } from "./connection-ui";
import { CollectionTable, inSet, useLocalView, type GridColumn } from "./data-table";
import { acknowledgeBudgetAlert, budgetRatio, budgetTone, scopeLabels, snoozeBudgetAlert } from "./budgets";
import { percent, usd } from "./gateway";
import type { Budget, BudgetAlert } from "./gen/blaxsmith/api/v1/gateway_pb";
import { Timestamp } from "./ui";

const monthEnd = (periodEnd: string) => new Date(`${periodEnd}T00:00:00Z`).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });

// Spend against the amount, the fired thresholds, and the month-end forecast.
export function BudgetMeter({ budget }: { budget: Budget }) {
  const ratio = budgetRatio(budget.spendUsdMicros, budget.amountUsdMicros);
  const tone = budgetTone(budget);
  const over = Number(budget.forecastUsdMicros) > Number(budget.amountUsdMicros);
  return <span className={`budget-meter tone-${tone}`}>
    <span className="budget-meter-line">
      <meter min={0} max={1} low={0.5} high={0.8} optimum={0} value={Math.min(ratio, 1)}
        aria-label={`${usd(budget.spendUsdMicros)} of ${usd(budget.amountUsdMicros)} spent, ${percent(ratio, 1)}`} />
      <strong className="num">{percent(ratio, 1)}</strong>
    </span>
    <small><span className="num">{usd(budget.spendUsdMicros)}</span> of <span className="num">{usd(budget.amountUsdMicros)}</span>
      {" · "}<span className={over ? "budget-forecast is-over" : "budget-forecast"}>at this rate {usd(budget.forecastUsdMicros)} by {monthEnd(budget.periodEnd)}</span></small>
  </span>;
}

export function BudgetTarget({ scope, projectId, projectName, principalName }: { scope: string; projectId: string; projectName: string; principalName: string }) {
  if (scope === "project") return <Link className="text-link" to="/projects/$projectId/usage" params={{ projectId }}>{projectName || "Deleted project"}</Link>;
  if (scope === "user") return <span>{principalName || "Unknown user"}</span>;
  return <span>Whole organization</span>;
}

const snoozeChoices = [{ hours: 24, label: "1 day" }, { hours: 168, label: "1 week" }];

// The alert feed: newest first, with acknowledge and snooze for open alerts.
export function AlertsTable({ id, alerts, loading, error, empty, invalidate, showTarget = true, canAct = true }: {
  id: string; alerts: BudgetAlert[]; loading?: boolean; error?: React.ReactNode; empty: React.ReactNode; invalidate: () => Promise<unknown>; showTarget?: boolean;
  canAct?: boolean; // Recipients only: owners, admins, the budget's user, the project's administrators.
}) {
  const queryClient = useQueryClient();
  const [view, setView] = useLocalView({ size: 20 });
  const [message, setMessage] = useState("");
  const act = useMutation({
    mutationFn: async ({ alert, hours }: { alert: BudgetAlert; hours?: number }) => { if (hours) await snoozeBudgetAlert(alert.id, hours); else await acknowledgeBudgetAlert(alert.id); },
    onSuccess: async (_, { hours }) => {
      setMessage(hours ? "Alert snoozed. It leaves the inbox until then." : "Alert acknowledged.");
      await Promise.all([invalidate(), queryClient.invalidateQueries({ queryKey: ["workspace-inbox"] }), queryClient.invalidateQueries({ queryKey: ["workspace-home"] })]);
    },
    onError: (cause) => setMessage(failure(cause, "The alert could not be updated.")),
  });
  const columns = useMemo<GridColumn<BudgetAlert>[]>(() => {
    const cols: GridColumn<BudgetAlert>[] = [
      { id: "alert", accessorKey: "budgetName", header: "Alert", enableHiding: false, cell: ({ row }) => <span className="task-stage">
        <strong>{row.original.budgetName} passed {row.original.thresholdPct}%</strong>
        <small><span className="num">{usd(row.original.spendUsdMicros)}</span> of <span className="num">{usd(row.original.amountUsdMicros)}</span> · forecast <span className="num">{usd(row.original.forecastUsdMicros)}</span></small></span> },
    ];
    if (showTarget) cols.push({ id: "scope", accessorKey: "scope", header: "Scope", filterFn: inSet, cell: ({ row }) => <span className="task-stage">
      <strong>{scopeLabels[row.original.scope] ?? row.original.scope}</strong><small><BudgetTarget {...row.original} /></small></span> });
    cols.push(
      { id: "threshold", accessorKey: "thresholdPct", header: "Threshold", cell: ({ row }) => <span className={`state-badge ${row.original.thresholdPct >= 100 ? "state-failed" : "state-waiting"}`}>{row.original.thresholdPct}%</span> },
      { id: "created", accessorKey: "createdAt", header: "Raised", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
      { id: "state", accessorFn: (a) => a.acknowledgedAt ? "acknowledged" : a.snoozedUntil && Date.parse(a.snoozedUntil) > Date.now() ? "snoozed" : "open", header: "State", filterFn: inSet,
        cell: ({ row }) => { const a = row.original;
          if (a.acknowledgedAt) return <span className="task-stage"><span className="state-badge state-succeeded">Acknowledged</span><small>{a.acknowledgedByUsername ? `@${a.acknowledgedByUsername} · ` : ""}<Timestamp value={a.acknowledgedAt} /></small></span>;
          if (a.snoozedUntil && Date.parse(a.snoozedUntil) > Date.now()) return <span className="task-stage"><span className="state-badge">Snoozed</span><small>until <Timestamp value={a.snoozedUntil} /></small></span>;
          return <span className="state-badge state-waiting">Open</span>; } },
      { id: "act", header: "Action", enableSorting: false, enableHiding: false, cell: ({ row }) => row.original.acknowledgedAt || !canAct ? <span className="muted">—</span>
        : <span className="row-actions">
          <button type="button" className="text-action" disabled={act.isPending} onClick={() => { setMessage(""); act.mutate({ alert: row.original }); }}>Acknowledge</button>
          {snoozeChoices.map((c) => <button key={c.hours} type="button" className="text-action" disabled={act.isPending} aria-label={`Snooze ${row.original.budgetName} ${row.original.thresholdPct}% alert for ${c.label}`}
            onClick={() => { setMessage(""); act.mutate({ alert: row.original, hours: c.hours }); }}>Snooze {c.label}</button>)}
        </span> },
    );
    return cols;
  }, [act, showTarget, canAct]);
  return <>
    <CollectionTable id={id} label="Budget alerts" columns={columns} data={alerts} getRowId={(a) => a.id} view={view} onView={setView}
      loading={loading} error={error} empty={empty} noun="alerts"
      facets={[{ id: "state", label: "State", options: [{ value: "open", label: "Open" }, { value: "snoozed", label: "Snoozed" }, { value: "acknowledged", label: "Acknowledged" }] },
        ...(showTarget ? [{ id: "scope", label: "Scope", options: Object.entries(scopeLabels).map(([value, label]) => ({ value, label })) }] : [])]} />
    {message ? <p className="card-note" role="status">{message}</p> : null}
  </>;
}
