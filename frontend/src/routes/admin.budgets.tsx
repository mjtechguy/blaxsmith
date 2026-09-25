import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { PiggyBank } from "lucide-react";
import { BudgetMeter, BudgetTarget } from "../budget-ui";
import { archiveBudget, budgetsKey, createBudget, dollarsToMicros, listBudgets, parseThresholds, scopeLabels, setBudgetsEnabled, updateBudget } from "../budgets";
import { ConfirmDialog, failure, useOrg } from "../connection-ui";
import { CollectionTable, inSet, useLocalView, type GridColumn } from "../data-table";
import { usd } from "../gateway";
import type { Budget } from "../gen/blaxsmith/api/v1/gateway_pb";
import { DashboardLayout } from "../layouts";
import { listMembers, membersKey } from "../users";
import { listProjects } from "../workflow";
import { Card, EmptyState, StatePanel, StatTile, Timestamp } from "../ui";
import { GatewayOff } from "../usage-ui";

export const Route = createFileRoute("/admin/budgets")({ component: BudgetsPage });

// Admin → Budgets (docs/model-gateway-plan.md §8, §9.1): monthly soft budgets
// per organization, project or user, with progress, forecast and thresholds.
// Crossing a threshold raises one alert, in the inbox and the audit log; nothing is blocked.
function BudgetsPage() {
  const { org } = useOrg();
  const queryClient = useQueryClient();
  const budgets = useQuery({ queryKey: budgetsKey(org), enabled: Boolean(org), refetchInterval: 60_000, queryFn: ({ signal }) => listBudgets(signal) });
  const invalidate = () => queryClient.invalidateQueries({ queryKey: budgetsKey(org) });
  const [switchError, setSwitchError] = useState("");
  const toggle = useMutation({ mutationFn: setBudgetsEnabled, onSuccess: invalidate, onError: (cause) => setSwitchError(failure(cause, "The switch could not be changed.")) });
  const title = "Budgets";
  if (budgets.isPending) return <DashboardLayout title={title}><StatePanel kind="loading" title="Loading budgets" /></DashboardLayout>;
  if (budgets.isError || !budgets.data) return <DashboardLayout title={title}><StatePanel kind="error" title="Budgets are unavailable" retry={() => void budgets.refetch()} /></DashboardLayout>;
  const data = budgets.data;
  if (!data.gatewayEnabled) return <DashboardLayout title={title}><GatewayOff admin /></DashboardLayout>;
  const list = data.budgets;
  const spend = list.find((b) => b.scope === "organization");
  const over = list.filter((b) => Number(b.spendUsdMicros) >= Number(b.amountUsdMicros)).length;
  const forecastOver = list.filter((b) => Number(b.forecastUsdMicros) > Number(b.amountUsdMicros)).length;
  return <DashboardLayout title={title} description="Monthly soft budgets in estimated USD (UTC calendar month). Crossing a threshold sends one alert to the inbox and the audit log. Budgets never block runs."
    tiles={<>
      <StatTile label="Organization budget" value={spend ? usd(spend.amountUsdMicros) : "Not set"} meta={spend ? `${usd(spend.spendUsdMicros)} spent this month` : "Add one below"} />
      <StatTile label="Budgets" value={list.length} meta={data.budgetsEnabled ? "Alerts on" : "Alerts off"} tone={data.budgetsEnabled ? undefined : "attention"} />
      <StatTile label="Over budget" value={over} tone={over ? "danger" : undefined} />
      <StatTile label="Forecast over" value={forecastOver} meta="At this month's run-rate" tone={forecastOver ? "attention" : undefined} />
    </>}>
    <div className="dash-wide">
      <Card title="Budgets & alerts" description="The organization switch for threshold alerts (Settings → Model gateway lists it too). While off, budgets still show progress but raise no alerts.">
        <ul className="flag-list budget-switch"><li className="flag-row">
          <div><label htmlFor="budgets-enabled">Raise budget alerts</label><p id="budgets-enabled-help">Checked after each usage rollup, about once a minute. Each threshold fires once per month. Changes are audited.</p></div>
          <input id="budgets-enabled" type="checkbox" role="switch" className="switch" aria-describedby="budgets-enabled-help" checked={data.budgetsEnabled} disabled={toggle.isPending}
            onChange={(e) => { setSwitchError(""); toggle.mutate(e.target.checked); }} />
        </li></ul>
        {switchError ? <p className="auth-alert" role="alert">{switchError}</p> : null}
      </Card>
    </div>
    <div className="dash-wide"><BudgetTable budgets={list} invalidate={invalidate} /></div>
    <div className="dash-wide"><NewBudget org={org} invalidate={invalidate} /></div>
  </DashboardLayout>;
}

function BudgetTable({ budgets, invalidate }: { budgets: Budget[]; invalidate: () => Promise<unknown> }) {
  const [view, setView] = useLocalView({ size: 20 });
  const [editing, setEditing] = useState<Budget | null>(null);
  const [archiving, setArchiving] = useState<Budget | null>(null);
  const archive = useMutation({ mutationFn: (b: Budget) => archiveBudget(b.id), onSuccess: async () => { setArchiving(null); await invalidate(); } });
  const columns = useMemo<GridColumn<Budget>[]>(() => [
    { id: "name", accessorKey: "name", header: "Budget", enableHiding: false, cell: ({ row }) => <span className="task-stage"><strong>{row.original.name}</strong>
      <small>{scopeLabels[row.original.scope]} · <BudgetTarget {...row.original} /></small></span> },
    { id: "scope", accessorKey: "scope", header: "Scope", filterFn: inSet, cell: ({ row }) => scopeLabels[row.original.scope] ?? row.original.scope },
    { id: "progress", accessorFn: (b) => Number(b.spendUsdMicros) / Math.max(1, Number(b.amountUsdMicros)), header: "This month", cell: ({ row }) => <BudgetMeter budget={row.original} /> },
    { id: "thresholds", header: "Thresholds", enableSorting: false, cell: ({ row }) => <span className="num">{row.original.thresholds.map((t) =>
      row.original.firedThresholds.includes(t) ? `${t}% ✓` : `${t}%`).join(" · ")}</span> },
    { id: "created", accessorKey: "createdAt", header: "Created", cell: ({ row }) => <Timestamp value={row.original.createdAt} /> },
    { id: "act", header: "Action", enableSorting: false, enableHiding: false, cell: ({ row }) => <span className="row-actions">
      <button type="button" className="text-action" onClick={() => setEditing(row.original)}>Edit</button>
      <button type="button" className="text-action text-action-danger" onClick={() => { archive.reset(); setArchiving(row.original); }}>Archive</button></span> },
  ], [archive]);
  return <Card title="All budgets" description="One active budget per organization, project or user. ✓ marks thresholds already alerted this month.">
    <CollectionTable id="gateway-budgets" label="Budgets" columns={columns} data={budgets} getRowId={(b) => b.id} view={view} onView={setView} noun="budgets"
      searchLabel="Search budgets" defaultHidden={["created", "scope"]}
      facets={[{ id: "scope", label: "Scope", options: Object.entries(scopeLabels).map(([value, label]) => ({ value, label })) }]}
      empty={<EmptyState icon={<PiggyBank size={22} aria-hidden="true" />} title="No budgets yet">Add an organization budget first, then project or user budgets where you want a closer watch.</EmptyState>} />
    {editing ? <EditBudget key={editing.id} budget={editing} onDone={async (saved) => { if (saved) await invalidate(); setEditing(null); }} /> : null}
    {archiving ? <ConfirmDialog title={`Archive ${archiving.name}?`} confirmLabel="Archive budget" busy={archive.isPending}
      error={archive.isError ? failure(archive.error, "The budget could not be archived.") : ""}
      body="It stops tracking and raises no more alerts. Its past alerts stay in the alert feed and the audit log."
      onConfirm={() => archive.mutate(archiving)} onClose={() => setArchiving(null)} /> : null}
  </Card>;
}

function EditBudget({ budget, onDone }: { budget: Budget; onDone: (saved: boolean) => Promise<void> }) {
  const [name, setName] = useState(budget.name);
  const [amount, setAmount] = useState((Number(budget.amountUsdMicros) / 1e6).toString());
  const [thresholds, setThresholds] = useState(budget.thresholds.join(", "));
  const save = useMutation({
    mutationFn: () => {
      const micros = dollarsToMicros(amount);
      const pcts = parseThresholds(thresholds);
      if (!name.trim() || micros === null || pcts === null) throw new Error("Enter a name, a positive amount, and 1–6 distinct thresholds between 1% and 200%.");
      return updateBudget(budget.id, { name: name.trim(), scope: budget.scope, projectId: budget.projectId, principalId: budget.principalId, amountUsdMicros: micros, thresholds: pcts }, budget.version);
    },
    onSuccess: () => onDone(true),
  });
  return <form className="budget-form" noValidate aria-label={`Edit ${budget.name}`} onSubmit={(e) => { e.preventDefault(); save.mutate(); }}>
    <p className="form-hint">Editing <strong>{budget.name}</strong>. Scope cannot change; archive and add a new budget instead. Raising the amount does not re-send alerts already raised this month.</p>
    <label className="form-field"><span>Name</span><input value={name} maxLength={120} onChange={(e) => setName(e.target.value)} /></label>
    <label className="form-field"><span>Monthly amount (USD)</span><input value={amount} inputMode="decimal" onChange={(e) => setAmount(e.target.value)} /></label>
    <label className="form-field"><span>Thresholds (%)</span><input value={thresholds} onChange={(e) => setThresholds(e.target.value)} /></label>
    <span className="row-actions"><button type="submit" className="primary-button" disabled={save.isPending}>{save.isPending ? "Saving…" : "Save budget"}</button>
      <button type="button" className="secondary-button" disabled={save.isPending} onClick={() => void onDone(false)}>Cancel</button></span>
    {save.isError ? <p className="auth-alert form-hint" role="alert">{save.error instanceof Error && !("code" in save.error) ? save.error.message : failure(save.error, "The budget could not be saved.")}</p> : null}
  </form>;
}

function NewBudget({ org, invalidate }: { org: string; invalidate: () => Promise<unknown> }) {
  const [draft, setDraft] = useState({ scope: "organization", projectId: "", principalId: "", name: "", amount: "", thresholds: "50, 80, 100" });
  const [projectSearch, setProjectSearch] = useState("");
  const [message, setMessage] = useState("");
  const projects = useQuery({ queryKey: ["budget-projects", org, projectSearch], enabled: Boolean(org) && draft.scope === "project",
    queryFn: ({ signal }) => listProjects("", projectSearch, "name", "asc", signal) });
  const members = useQuery({ queryKey: membersKey(org), enabled: Boolean(org) && draft.scope === "user", queryFn: ({ signal }) => listMembers(signal) });
  const create = useMutation({
    mutationFn: () => {
      const micros = dollarsToMicros(draft.amount);
      const pcts = parseThresholds(draft.thresholds);
      if (!draft.name.trim() || micros === null || pcts === null) throw new Error("Enter a name, a positive monthly amount, and 1–6 distinct thresholds between 1% and 200%.");
      if (draft.scope === "project" && !draft.projectId) throw new Error("Choose the project this budget covers.");
      if (draft.scope === "user" && !draft.principalId) throw new Error("Choose the user this budget covers.");
      return createBudget({ name: draft.name.trim(), scope: draft.scope, projectId: draft.scope === "project" ? draft.projectId : "",
        principalId: draft.scope === "user" ? draft.principalId : "", amountUsdMicros: micros, thresholds: pcts });
    },
    onSuccess: async (res) => { setMessage(`Budget “${res.budget?.name}” added.`); setDraft({ ...draft, name: "", amount: "", projectId: "", principalId: "" }); await invalidate(); },
    onError: (cause) => setMessage(cause instanceof Error && !("code" in cause) ? cause.message : failure(cause, "The budget could not be added.")),
  });
  const set = (patch: Partial<typeof draft>) => setDraft({ ...draft, ...patch });
  return <Card title="Add a budget" description="Choose what it covers, a monthly amount in estimated USD, and the percentages that raise an alert. Owners and admins receive every alert; a user budget also alerts that user, a project budget the project's administrators.">
    <form className="budget-form" noValidate aria-label="Add a budget" onSubmit={(e) => { e.preventDefault(); setMessage(""); create.mutate(); }}>
      <label className="form-field"><span>Scope</span><select value={draft.scope} onChange={(e) => set({ scope: e.target.value, projectId: "", principalId: "" })}>
        <option value="organization">Organization</option><option value="project">Project</option><option value="user">User</option></select></label>
      {draft.scope === "project" ? <>
        <label className="form-field"><span>Find a project</span><input type="search" value={projectSearch} placeholder="Name or slug" onChange={(e) => setProjectSearch(e.target.value)} /></label>
        <label className="form-field"><span>Project</span><select value={draft.projectId} onChange={(e) => set({ projectId: e.target.value })}>
          <option value="">{projects.isPending ? "Loading…" : "Choose a project"}</option>
          {(projects.data?.projects ?? []).map((p) => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label>
      </> : null}
      {draft.scope === "user" ? <label className="form-field"><span>User</span><select value={draft.principalId} onChange={(e) => set({ principalId: e.target.value })}>
        <option value="">{members.isPending ? "Loading…" : "Choose a user"}</option>
        {(members.data?.members ?? []).filter((m) => m.status !== "invited").map((m) => <option key={m.principalId} value={m.principalId}>{m.displayName || m.username}</option>)}</select></label> : null}
      <label className="form-field"><span>Name</span><input value={draft.name} maxLength={120} placeholder="Engineering monthly" onChange={(e) => set({ name: e.target.value })} /></label>
      <label className="form-field"><span>Monthly amount (USD)</span><input value={draft.amount} inputMode="decimal" placeholder="500" onChange={(e) => set({ amount: e.target.value })} /></label>
      <label className="form-field"><span>Thresholds (%)</span><input value={draft.thresholds} onChange={(e) => set({ thresholds: e.target.value })} /></label>
      <button type="submit" className="primary-button" disabled={create.isPending}>{create.isPending ? "Adding…" : "Add budget"}</button>
    </form>
    {message ? <p className="card-note" role="status">{message}</p> : null}
    <p className="card-note">Alerts appear in <Link className="text-link" to="/admin/alerts">Admin → Alerts</Link> and each recipient's inbox.</p>
  </Card>;
}
