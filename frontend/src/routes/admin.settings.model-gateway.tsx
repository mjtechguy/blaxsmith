import { useMemo, useState, type ReactNode } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useForm } from "@tanstack/react-form";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { AlertTriangle, Waypoints } from "lucide-react";
import { useOrg } from "../connection-ui";
import { CollectionTable, useLocalView, type GridColumn } from "../data-table";
import { deliveryLabels, gatewaySettingsKey, gatewayStatusKey, getGatewaySettings, listModelPrices, pricesKey, rate, setModelPriceOverride, updateGatewaySettings } from "../gateway";
import type { GetGatewaySettingsResponse, ModelPrice } from "../gen/blaxsmith/api/v1/gateway_pb";
import { GuardedSaveBar, useSaved } from "../layouts";
import { Card, StatePanel, Timestamp } from "../ui";

export const Route = createFileRoute("/admin/settings/model-gateway")({ component: ModelGatewaySettings });

// Later-phase switches (§15.1) are shown so admins know what is coming; they
// cannot be turned on in G1.
const laterFlags = [
  { id: "pools", label: "Pools & failover", phase: "G2", text: "Group API-key and cloud routes into pools with health-based selection and failover before the first byte. Off: one route per connection." },
  { id: "pacing", label: "Rate-aware pacing", phase: "G2", text: "Queue stages until a pool has headroom instead of failing them, with a visible reset countdown." },
  { id: "budgets", label: "Budgets & alerts", phase: "G3", text: "Soft budgets per organization, project or user with threshold alerts. No blocking." },
  { id: "personal", label: "Personal subscription routes", phase: "G4", text: "Each member's own subscription connection as a route for their own runs only. Never pooled or shared." },
  { id: "content", label: "Content capture", phase: "a later phase", text: "Store redacted, encrypted request and response bodies for a limited retention. Off: only counts and metadata are recorded." },
];

// Later-phase flags that are now real switches in the editor above.
const shipped = new Set(["pools", "pacing", "personal"]);

function ModelGatewaySettings() {
  const { org } = useOrg();
  const [saved, setSaved] = useSaved();
  const settings = useQuery({ queryKey: gatewaySettingsKey(org), enabled: Boolean(org), queryFn: ({ signal }) => getGatewaySettings(signal) });
  if (settings.isPending) return <StatePanel kind="loading" title="Loading model gateway settings" />;
  if (settings.isError) return <StatePanel kind="error" title="Model gateway settings unavailable" retry={() => void settings.refetch()} />;
  return <>
    <SettingsEditor key={settings.data.version.toString()} data={settings.data} org={org} saved={saved} setSaved={setSaved} />
    <LaterPhases />
    <Prices org={org} />
  </>;
}

function Flag({ id, label, checked, disabled, onChange, children }: { id: string; label: string; checked: boolean; disabled?: boolean; onChange: (v: boolean) => void; children: ReactNode }) {
  return <li className={`flag-row${disabled ? " is-disabled" : ""}`}>
    <div><label htmlFor={id}>{label}</label>{children}</div>
    <input id={id} type="checkbox" role="switch" className="switch" checked={checked} disabled={disabled} onChange={(e) => onChange(e.target.checked)} aria-describedby={`${id}-help`} />
  </li>;
}

function SettingsEditor({ data, org, saved, setSaved }: { data: GetGatewaySettingsResponse; org: string; saved: boolean; setSaved: (v: boolean) => void }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState("");
  const s = data.settings!;
  const form = useForm({
    defaultValues: { enabled: s.enabled, defaultDeliveryMode: s.defaultDeliveryMode || "native_raw", allowProjectChoice: s.allowProjectChoice, removeDirectEgress: s.removeDirectEgress,
      poolsEnabled: s.poolsEnabled, pacingEnabled: s.pacingEnabled, personalRoutesEnabled: s.personalRoutesEnabled, eventRetentionDays: String(s.eventRetentionDays || 90) },
    onSubmit: async ({ value }) => {
      setError("");
      const days = Number(value.eventRetentionDays);
      if (!Number.isInteger(days) || days < 7 || days > 400) {
        setError("Keep raw usage events for 7 to 400 days.");
        return;
      }
      try {
        await updateGatewaySettings({ ...value, eventRetentionDays: days }, data.version);
        await Promise.all([queryClient.invalidateQueries({ queryKey: gatewaySettingsKey(org) }), queryClient.invalidateQueries({ queryKey: gatewayStatusKey(org) })]);
        setSaved(true);
      } catch (cause) {
        const code = ConnectError.from(cause).code;
        setError(code === Code.Aborted ? "Someone else changed these settings. Reload the page to see their changes."
          : code === Code.FailedPrecondition ? "This installation does not run the model gateway. An operator must enable it in the Helm values (gateway.enabled) first."
            : code === Code.PermissionDenied ? "Only organization owners and admins can change these settings." : "The settings could not be saved. Please try again.");
      }
    },
  });
  return <form className="settings-form" noValidate onSubmit={(event) => { event.preventDefault(); event.stopPropagation(); void form.handleSubmit(); }}>
    <section className="editor-card" aria-labelledby="gateway-heading">
      <div className="editor-card-heading"><span className="project-symbol"><Waypoints size={18} aria-hidden="true" /></span><div><h2 id="gateway-heading">Model gateway</h2>
        <p>Runs call models through Blaxsmith with a short-lived, run-scoped token instead of receiving the provider key. The gateway injects the key, meters usage and never stores prompts or responses. Off by default; with it off, runs keep receiving keys directly.</p></div></div>
      {!data.installationAvailable ? <p className="flag-warning" role="note"><AlertTriangle size={14} aria-hidden="true" /> This installation does not run the gateway service. An operator enables it with the Helm value <code>gateway.enabled</code>; until then the switch stays off.</p> : null}
      <form.Subscribe selector={(state) => state.values}>{(v) => <ul className="flag-list">
        <form.Field name="enabled">{(field) => <Flag id="gw-enabled" label="Enable model gateway" checked={field.state.value}
          disabled={!data.installationAvailable && !field.state.value} onChange={field.handleChange}>
          <p id="gw-enabled-help">The master switch. When off, no gateway tokens are issued, the gateway rejects this organization's tokens, and gateway pages other than this one are hidden. Turning it off drains cleanly: streams in progress finish, the next request fails with "gateway disabled by your admin", and projects fall back to direct keys on their next run.</p>
        </Flag>}</form.Field>
        <li className="flag-row"><div><label htmlFor="gw-default">Default delivery mode for new projects</label>
          <p id="gw-default-help">How runs receive model access unless their project chooses otherwise.</p></div>
          <form.Field name="defaultDeliveryMode">{(field) => <span className="form-field"><select id="gw-default" aria-describedby="gw-default-help" value={field.state.value} onChange={(e) => field.handleChange(e.target.value)}>
            <option value="native_raw">{deliveryLabels.native_raw}</option><option value="brokered_gateway">{deliveryLabels.brokered_gateway}</option></select></span>}</form.Field></li>
        <form.Field name="allowProjectChoice">{(field) => <Flag id="gw-choice" label="Allow projects to choose delivery mode" checked={field.state.value} onChange={field.handleChange}>
          <p id="gw-choice-help">Project admins can switch a project between modes in Project → Settings → Model access. When off, the organization default is enforced.</p>
        </Flag>}</form.Field>
        <form.Field name="removeDirectEgress">{(field) => <Flag id="gw-egress" label="Remove direct provider egress for gateway runs" checked={field.state.value} onChange={field.handleChange}>
          <p id="gw-egress-help">Gateway-mode sandboxes can reach Git and the gateway, not provider hosts, so a leaked token is the only credential in reach.</p>
          {!v.removeDirectEgress ? <p className="flag-warning"><AlertTriangle size={14} aria-hidden="true" /> Direct egress stays open for gateway runs. Use this only for debugging.</p> : null}
        </Flag>}</form.Field>
        <form.Field name="poolsEnabled">{(field) => <Flag id="gw-pools" label="Pools & failover" checked={field.state.value} onChange={field.handleChange}>
          <p id="gw-pools-help">Pools of organization API-key and cloud routes, with health-based selection and failover before the first byte on 429s and errors. Set them up in <Link className="text-link" to="/admin/routes">Routes & pools</Link>. Off: one route per connection. Members' own subscriptions are never pooled.</p>
        </Flag>}</form.Field>
        <form.Field name="pacingEnabled">{(field) => <Flag id="gw-pacing" label="Rate-aware pacing" checked={field.state.value} onChange={field.handleChange}>
          <p id="gw-pacing-help">A stage whose pool has no headroom stays queued with a visible reset countdown instead of failing.</p>
        </Flag>}</form.Field>
        <form.Field name="personalRoutesEnabled">{(field) => <Flag id="gw-personal" label="Personal subscription routes" checked={field.state.value} onChange={field.handleChange}>
          <p id="gw-personal-help">A member's own Codex sign-in is served through the gateway for runs they start, so the sandbox never holds even an access token, and they see their own limit and reset meters in My usage. One account per route, owner-only, never pooled, shared or rotated. Members' own Claude setup-tokens always go through the gateway in gateway-mode projects.</p>
        </Flag>}</form.Field>
        <li className="flag-row"><div><label htmlFor="gw-retention">Raw usage event retention (days)</label>
          <p id="gw-retention-help">How long per-request usage events are kept, 7 to 400 days. Daily rollups and run totals are kept.</p></div>
          <form.Field name="eventRetentionDays">{(field) => <span className="form-field"><input id="gw-retention" inputMode="numeric" aria-describedby="gw-retention-help"
            value={field.state.value} onChange={(e) => field.handleChange(e.target.value)} /></span>}</form.Field></li>
      </ul>}</form.Subscribe>
      {data.updatedAt ? <p className="form-hint">Last changed <Timestamp value={data.updatedAt} />{data.updatedByUsername ? ` by @${data.updatedByUsername}` : ""}. Every change is recorded in the audit log.</p> : null}
    </section>
    <form.Subscribe selector={(state) => [state.isDirty, state.isSubmitting, state.canSubmit] as const}>
      {([dirty, submitting, canSubmit]) => <GuardedSaveBar dirty={dirty} saving={submitting} canSave={canSubmit} error={error} saved={saved} onCancel={() => { form.reset(); setError(""); }} />}
    </form.Subscribe>
  </form>;
}

function LaterPhases() {
  return <>
    <section className="editor-card" aria-labelledby="gateway-later-heading">
      <div className="editor-card-heading"><div><h2 id="gateway-later-heading">Later phases</h2><p>Planned switches, shown so you can see what is coming. None of them can be turned on yet.</p></div></div>
      <ul className="flag-list">{laterFlags.filter((flag) => !shipped.has(flag.id)).map((flag) => <li key={flag.id} className="flag-row is-disabled">
        <div><span className="flag-title" id={`later-${flag.id}`}>{flag.label} <span className="soon-badge">Coming in {flag.phase}</span></span><p>{flag.text}</p></div>
        <input type="checkbox" role="switch" className="switch" checked={false} disabled aria-labelledby={`later-${flag.id}`} readOnly /></li>)}</ul>
    </section>
  </>;
}

const dollarsToMicros = (value: string) => {
  const n = Number(value);
  return Number.isFinite(n) && n >= 0 && n <= 1000 && value.trim() !== "" ? BigInt(Math.round(n * 1e6)) : null;
};

function Prices({ org }: { org: string }) {
  const queryClient = useQueryClient();
  const prices = useQuery({ queryKey: pricesKey(org), enabled: Boolean(org), queryFn: ({ signal }) => listModelPrices(signal) });
  const [view, setView] = useLocalView({ size: 10 });
  const [draft, setDraft] = useState({ provider: "anthropic", model: "", input: "", output: "", cacheRead: "", cacheWrite: "" });
  const [message, setMessage] = useState("");
  const save = useMutation({
    mutationFn: () => {
      const [input, output, cacheRead, cacheWrite] = [draft.input, draft.output, draft.cacheRead, draft.cacheWrite].map(dollarsToMicros);
      if (!draft.model.trim() || input === null || output === null || cacheRead === null || cacheWrite === null) throw new Error("Enter a model and four rates between $0 and $1,000 per million tokens.");
      return setModelPriceOverride({ provider: draft.provider, model: draft.model.trim(), inputMicrosPerMtok: input, outputMicrosPerMtok: output, cacheReadMicrosPerMtok: cacheRead, cacheWriteMicrosPerMtok: cacheWrite });
    },
    onSuccess: async () => { setMessage("Override saved. It applies to requests from now on."); setDraft({ ...draft, model: "", input: "", output: "", cacheRead: "", cacheWrite: "" }); await queryClient.invalidateQueries({ queryKey: pricesKey(org) }); },
    onError: (cause) => setMessage(cause instanceof ConnectError ? "The override could not be saved. Check the model id and rates." : cause instanceof Error ? cause.message : "The override could not be saved."),
  });
  const columns = useMemo<GridColumn<ModelPrice>[]>(() => [
    { id: "model", accessorFn: (p) => `${p.provider}/${p.model}`, header: "Model", enableHiding: false, cell: ({ row }) => <span className="task-stage"><strong className="mono">{row.original.model}</strong><small>{row.original.provider}</small></span> },
    { id: "input", accessorFn: (p) => Number(p.inputMicrosPerMtok), header: "Input", cell: ({ row }) => <span className="num">{rate(row.original.inputMicrosPerMtok)}</span> },
    { id: "output", accessorFn: (p) => Number(p.outputMicrosPerMtok), header: "Output", cell: ({ row }) => <span className="num">{rate(row.original.outputMicrosPerMtok)}</span> },
    { id: "cacheRead", accessorFn: (p) => Number(p.cacheReadMicrosPerMtok), header: "Cache read", cell: ({ row }) => <span className="num">{rate(row.original.cacheReadMicrosPerMtok)}</span> },
    { id: "cacheWrite", accessorFn: (p) => Number(p.cacheWriteMicrosPerMtok), header: "Cache write", cell: ({ row }) => <span className="num">{rate(row.original.cacheWriteMicrosPerMtok)}</span> },
    { id: "source", accessorKey: "source", header: "Source", cell: ({ row }) => row.original.source === "override"
      ? <span className="state-badge state-active">Override · <Timestamp value={row.original.effectiveFrom} /></span> : <span className="state-badge">List price</span> },
  ], []);
  const field = (key: keyof typeof draft, label: string, placeholder: string) => <label className="form-field"><span>{label}</span>
    <input value={draft[key]} inputMode="decimal" placeholder={placeholder} onChange={(e) => setDraft({ ...draft, [key]: e.target.value })} /></label>;
  return <Card title="Prices" description="USD per million tokens used for estimated costs: bundled list prices, or your contracted rates as overrides. Costs are always estimates, not your provider bill. Models without a price are metered with no cost until you add one.">
    <CollectionTable id="gateway-prices" label="Model prices" columns={columns} data={prices.data?.prices ?? []} getRowId={(p) => `${p.provider}/${p.model}`}
      view={view} onView={setView} searchLabel="Search models" loading={prices.isPending} noun="models"
      error={prices.isError ? <>Prices are unavailable. <button type="button" className="text-action" onClick={() => void prices.refetch()}>Try again</button></> : undefined}
      empty="No prices yet." />
    <form className="price-form" noValidate onSubmit={(e) => { e.preventDefault(); setMessage(""); save.mutate(); }} aria-label="Add a price override">
      <label className="form-field"><span>Provider</span><select value={draft.provider} onChange={(e) => setDraft({ ...draft, provider: e.target.value })}>
        <option value="anthropic">Anthropic</option><option value="openai">OpenAI</option><option value="opencode">OpenCode Zen</option><option value="opencode-go">OpenCode Go</option></select></label>
      {field("model", "Model id", "claude-opus-5-5")}
      {field("input", "Input $/MTok", "4.00")}
      {field("output", "Output $/MTok", "20.00")}
      {field("cacheRead", "Cache read $/MTok", "0.20")}
      {field("cacheWrite", "Cache write $/MTok", "5.00")}
      <button type="submit" className="secondary-button" disabled={save.isPending}>{save.isPending ? "Saving…" : "Save override"}</button>
    </form>
    {message ? <p className="card-note" role="status">{message}</p> : null}
  </Card>;
}
