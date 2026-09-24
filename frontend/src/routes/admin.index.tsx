import { useEffect, useMemo, useRef, useState } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { Activity, ArrowRight, BookCopy, Inbox, KeyRound, OctagonX, RefreshCw, ScrollText, ShieldAlert, Square, Trash2, Users } from "lucide-react";
import { ADMIN_REFRESH_MS, adminOverviewKey, ago, getAdminOverview, haltRun, isOrgAdmin, revokeGrant } from "../admin";
import { currentSession, sessionQueryKey } from "../auth";
import { DataTable } from "../data-table";
import type { AdminConnection, AdminGrant, AdminLiveAttempt, AdminOpenInteraction } from "../gen/blaxsmith/api/v1/admin_pb";
import { PageHeader, PageShell } from "../page";
import { steerAttempt } from "../run-control";
import type { AgentStatus } from "../agent-view";
import { StatusPill, useAttentionTitle } from "../work-log";

// Live-agent pill: an open approval or question on the attempt's stage outranks its runtime state.
function liveStatus(attempt: AdminLiveAttempt, open: AdminOpenInteraction[]): AgentStatus | null {
  const mine = open.filter((item) => item.runId === attempt.runId && item.stage === attempt.stage);
  if (mine.some((item) => item.kind === "approval")) return "needs_approval";
  if (mine.length) return "awaiting_input";
  return ["reserved", "starting", "running", "reconciling"].includes(attempt.state) ? "working" : null;
}

export const Route = createFileRoute("/admin/")({ component: AdminOverview });

type PendingAction =
  | { kind: "stop"; attempt: AdminLiveAttempt }
  | { kind: "halt"; runId: string; label: string }
  | { kind: "revoke"; grant: AdminGrant };

const features = tableFeatures({});
const stateLabels: Record<string, string> = {
  running: "Running", waiting_on_human: "Waiting on human", escalated: "Escalated",
  failed: "Failed", succeeded: "Succeeded", halted: "Halted",
};
const stateBadge: Record<string, string> = {
  running: "state-running", waiting_on_human: "state-waiting", escalated: "state-blocked",
  failed: "state-failed", succeeded: "state-succeeded", halted: "state-cancelled",
};

function RunLink({ projectId, runId, label, stage, inbox }: { projectId: string; runId: string; label: string; stage?: string; inbox?: boolean }) {
  return <Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId, runId }}
    search={stage ? { stage } : {}} hash={inbox ? "inbox-heading" : undefined}>{label} <ArrowRight size={13} aria-hidden="true" /></Link>;
}

function AdminOverview() {
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const org = session.data?.organizationId || "";
  const allowed = isOrgAdmin(session.data);
  const overview = useQuery({
    queryKey: adminOverviewKey(org), enabled: Boolean(org && allowed),
    queryFn: ({ signal }) => getAdminOverview(signal), refetchInterval: ADMIN_REFRESH_MS,
  });
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => { setNow(Date.now()); }, [overview.dataUpdatedAt]);
  const dialog = useRef<HTMLDialogElement>(null);
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [reason, setReason] = useState("");
  const [actionError, setActionError] = useState("");
  const act = useMutation({
    mutationFn: async (action: PendingAction) => {
      if (action.kind === "stop") return steerAttempt(action.attempt.attemptId, "halt", { reason: reason.trim() });
      if (action.kind === "halt") return haltRun(action.runId);
      return revokeGrant(action.grant.id);
    },
    onSuccess: async () => {
      setPending(null);
      await queryClient.invalidateQueries({ queryKey: adminOverviewKey(org) });
    },
    onError: (cause) => {
      const code = ConnectError.from(cause).code;
      setActionError(code === Code.PermissionDenied ? "Your session is not allowed to do this."
        : code === Code.NotFound ? "This item no longer exists. The dashboard will refresh."
          : code === Code.FailedPrecondition ? "The item changed state (it may have already finished). The dashboard will refresh."
            : code === Code.Unauthenticated ? "Your session changed. Sign in again and retry."
              : "The action could not be completed. Please try again.");
      void queryClient.invalidateQueries({ queryKey: adminOverviewKey(org) });
    },
  });
  const busy = act.isPending;
  const ask = (action: PendingAction) => {
    setActionError("");
    setReason(action.kind === "stop" ? "Stopped by an administrator from the admin dashboard." : "");
    setPending(action);
  };
  useEffect(() => {
    if (pending) dialog.current?.showModal();
    else if (dialog.current?.open) dialog.current.close();
  }, [pending]);

  const openItems = overview.data?.openInteractions;
  useAttentionTitle((openItems ?? []).length);
  const liveColumns = useMemo<ColumnDef<typeof features, AdminLiveAttempt>[]>(() => [
    { id: "run", header: "Project / run", cell: ({ row }) => <span className="task-stage"><strong>{row.original.projectName}</strong>
      <RunLink projectId={row.original.projectId} runId={row.original.runId} label={row.original.runLaunchKey} stage={row.original.stage} /></span> },
    { id: "stage", header: "Stage", cell: ({ row }) => <span className="task-stage"><strong>{row.original.stage}</strong><small>{row.original.kind.replaceAll("_", " ") || "—"}</small></span> },
    { id: "runtime", header: "Harness / model", cell: ({ row }) => <span className="task-stage"><strong>{row.original.harness || "—"}</strong><small className="mono">{row.original.model || "—"}</small></span> },
    { id: "status", header: "Status", cell: ({ row }) => <StatusPill status={liveStatus(row.original, openItems ?? [])} /> },
    { id: "state", header: "State", cell: ({ row }) => <span className={`state-badge state-${row.original.state}`}>{row.original.state}</span> },
    { id: "control", header: "Control", cell: ({ row }) => row.original.controllerPrincipalId
      ? <span className="control-pill control-other" title={row.original.controllerPrincipalId}>Taken over · {row.original.controllerUsername || row.original.controllerPrincipalId.slice(0, 8)}</span>
      : <span className="control-pill control-agent">Agent</span> },
    { id: "started", header: "Started", cell: ({ row }) => <time dateTime={row.original.startedAt} title={new Date(row.original.startedAt).toLocaleString()}>{ago(row.original.startedAt, now)}</time> },
    { id: "activity", header: "Last activity", cell: ({ row }) => row.original.lastActivityAt
      ? <time dateTime={row.original.lastActivityAt} title={new Date(row.original.lastActivityAt).toLocaleString()}>{ago(row.original.lastActivityAt, now)}</time> : "—" },
    { id: "actions", header: "Actions", cell: ({ row }) => <span className="admin-actions">
      <button type="button" className="text-action text-action-danger" disabled={busy || row.original.state !== "running"}
        title={row.original.state !== "running" ? "Only a running attempt can be asked to stop" : undefined}
        onClick={() => ask({ kind: "stop", attempt: row.original })} aria-label={`Stop attempt on stage ${row.original.stage} of ${row.original.runLaunchKey}`}><Square size={13} aria-hidden="true" /> Stop</button>
      <button type="button" className="text-action text-action-danger" disabled={busy}
        onClick={() => ask({ kind: "halt", runId: row.original.runId, label: `${row.original.projectName} / ${row.original.runLaunchKey}` })}
        aria-label={`Halt run ${row.original.runLaunchKey}`}><OctagonX size={13} aria-hidden="true" /> Halt run</button>
    </span> },
  ], [busy, now, openItems]);

  const inboxColumns = useMemo<ColumnDef<typeof features, AdminOpenInteraction>[]>(() => [
    { id: "age", header: "Waiting", cell: ({ row }) => <time dateTime={row.original.createdAt} title={new Date(row.original.createdAt).toLocaleString()}>{ago(row.original.createdAt, now)}</time> },
    { id: "kind", header: "Kind", cell: ({ row }) => <span className={`state-badge ${row.original.kind === "escalation" ? "state-blocked" : "state-waiting"}`}>{row.original.kind.replaceAll("_", " ")}</span> },
    { id: "title", header: "Question", cell: ({ row }) => <span className="admin-wrap"><strong>{row.original.title}</strong>{row.original.blocking ? <small> · blocking</small> : null}</span> },
    { id: "where", header: "Project / run · stage", cell: ({ row }) => <span className="task-stage"><strong>{row.original.projectName} · {row.original.stage}</strong>
      <RunLink projectId={row.original.projectId} runId={row.original.runId} label={`${row.original.runLaunchKey} inbox`} inbox /></span> },
  ], [now]);

  const connectionColumns = useMemo<ColumnDef<typeof features, AdminConnection>[]>(() => [
    { id: "provider", header: "Provider", cell: ({ row }) => <span className="task-stage"><strong>{row.original.providerKind === "git" ? "Git" : row.original.providerKind === "openai" ? "OpenAI" : row.original.providerKind === "anthropic" ? "Anthropic" : row.original.providerKind}</strong><small>{row.original.host}</small></span> },
    { id: "account", header: "Owner / account", cell: ({ row }) => <span className="task-stage"><strong>{row.original.ownerKind === "organization" ? "Organization" : "User"}</strong><small className="mono">{row.original.account}</small></span> },
    { id: "state", header: "State", cell: ({ row }) => <span className={`state-badge ${row.original.state === "active" ? "state-succeeded" : "state-failed"}`}>{row.original.state}</span> },
    { id: "grants", header: "Grants", cell: ({ row }) => row.original.activeGrants },
    { id: "leases", header: "Leases", cell: ({ row }) => <span>{row.original.activeLeases} live{row.original.expiringLeases ? <span className="state-badge state-waiting admin-inline-badge">{row.original.expiringLeases} expiring</span> : null}</span> },
    { id: "used", header: "Last used", cell: ({ row }) => row.original.lastUsedAt ? <time dateTime={row.original.lastUsedAt} title={new Date(row.original.lastUsedAt).toLocaleString()}>{ago(row.original.lastUsedAt, now)}</time> : "Never" },
  ], [now]);

  const grantColumns = useMemo<ColumnDef<typeof features, AdminGrant>[]>(() => [
    { id: "project", header: "Project", cell: ({ row }) => <strong>{row.original.projectName || row.original.projectId.slice(0, 8)}</strong> },
    { id: "capability", header: "Capability", cell: ({ row }) => <span className="mono">{row.original.capability}</span> },
    { id: "resource", header: "Resource", cell: ({ row }) => <span className="mono admin-wrap">{row.original.resource}</span> },
    { id: "expires", header: "Expires", cell: ({ row }) => row.original.expiresAt ? new Date(row.original.expiresAt).toLocaleString() : "Standing" },
    { id: "created", header: "Granted", cell: ({ row }) => <time dateTime={row.original.createdAt}>{ago(row.original.createdAt, now)}</time> },
    { id: "actions", header: "Actions", cell: ({ row }) => <button type="button" className="text-action text-action-danger" disabled={busy}
      onClick={() => ask({ kind: "revoke", grant: row.original })} aria-label={`Revoke ${row.original.capability} grant for ${row.original.projectName}`}><Trash2 size={13} aria-hidden="true" /> Revoke</button> },
  ], [busy, now]);

  const data = overview.data;
  const live = useTable({ features, data: data?.liveAttempts || [], columns: liveColumns, getRowId: (row) => row.attemptId });
  const inbox = useTable({ features, data: data?.openInteractions || [], columns: inboxColumns, getRowId: (row) => row.id });
  const connections = useTable({ features, data: data?.connections || [], columns: connectionColumns, getRowId: (row) => row.id });
  const grants = useTable({ features, data: data?.grants || [], columns: grantColumns, getRowId: (row) => row.id });

  if (session.data && !allowed) return <PageShell><div className="state-panel" role="alert"><ShieldAlert size={22} aria-hidden="true" /><h2>Administration is restricted</h2><p>Only organization owners and admins can open the admin dashboard.</p><Link className="secondary-button" to="/">Back to workspace</Link></div></PageShell>;
  const denied = overview.isError && ConnectError.from(overview.error).code === Code.PermissionDenied;
  const capacity = data?.capacity;

  return <PageShell>
    <PageHeader eyebrow="Administration" title="Operations" description="What is running, what is waiting on a person, and connection health across every project in this organization."
      actions={<><Link className="secondary-button" to="/admin/users"><Users size={15} aria-hidden="true" /> Users</Link><Link className="secondary-button" to="/admin/recipes"><BookCopy size={15} aria-hidden="true" /> Recipes</Link><Link className="secondary-button" to="/admin/audit"><ScrollText size={15} aria-hidden="true" /> Audit log</Link></>} />
    {overview.isPending ? <div className="state-panel" role="status"><RefreshCw className="spin" size={22} aria-hidden="true" /><h2>Loading operations</h2></div> : null}
    {overview.isError ? <div className="state-panel" role="alert"><h2>{denied ? "Administration is restricted" : "Operations unavailable"}</h2><p>{denied ? "Your session is not an organization owner or admin." : "The admin overview could not be loaded."}</p>{denied ? null : <button className="secondary-button" type="button" onClick={() => void overview.refetch()}>Try again</button>}</div> : null}
    {data ? <>
      <section aria-labelledby="run-states-heading">
        <h2 id="run-states-heading" className="sr-only">Runs by state</h2>
        <div className="summary-grid admin-tiles">
          {data.runStates.map((entry) => <div key={entry.state} className="summary-card">
            <span className="summary-label">{stateLabels[entry.state] ?? entry.state}</span>
            <strong>{entry.count}</strong>
            <span className="summary-meta"><span className={`admin-dot ${stateBadge[entry.state] ?? ""}`} aria-hidden="true" /> last 24h + open</span>
          </div>)}
          <div className="summary-card">
            <span className="summary-label">Capacity</span>
            <strong>{capacity?.inFlight ?? 0}{capacity?.configuredMax ? <small className="admin-capacity-max"> / {capacity.configuredMax}</small> : null}</strong>
            <span className="summary-meta">{capacity?.running ?? 0} running · {capacity?.takenOver ?? 0} taken over{capacity?.workerPool ? ` · pool ${capacity.workerPool}` : ""}</span>
          </div>
        </div>
        {capacity && !capacity.configuredMax ? <p className="admin-note">Capacity counts in-flight attempts. Worker pool replicas are not readable by the app; set <code>BLAXSMITH_ADMIN_ATTEMPT_CAPACITY</code> to show a maximum.</p> : null}
      </section>

      <section className="table-section" aria-labelledby="live-heading">
        <div className="table-heading"><div><h2 id="live-heading"><Activity size={15} aria-hidden="true" /> Live agents</h2><p>Current attempt owners across projects. Stop asks the agent to halt at its next safe point; Halt run cancels the whole run and stops its workers.</p></div>
          <span className="fetched-time" role="status" aria-live="polite">{overview.isRefetching ? "Refreshing…" : `Updated ${ago(new Date(overview.dataUpdatedAt).toISOString(), now)}`}</span></div>
        <DataTable table={live} label="Live agents" empty="No agents are running." />
      </section>

      <section className="table-section" aria-labelledby="admin-inbox-heading">
        <div className="table-heading"><div><h2 id="admin-inbox-heading"><Inbox size={15} aria-hidden="true" /> Waiting on a person</h2><p>Open questions, approvals, and escalations on active runs, oldest first.</p></div><span className="fetched-time">{data.openInteractions.length} open</span></div>
        <DataTable table={inbox} label="Open interactions" empty="Nothing is waiting on a person." />
      </section>

      <section className="table-section" aria-labelledby="connections-heading">
        <div className="table-heading"><div><h2 id="connections-heading"><KeyRound size={15} aria-hidden="true" /> Connections</h2><p>Model and Git connections with grant and lease health. Credentials are never shown.</p></div><span className="fetched-time">{data.connections.length} connections</span></div>
        <DataTable table={connections} label="Connections" empty="No model or Git connections yet." />
      </section>

      <section className="table-section" aria-labelledby="grants-heading">
        <div className="table-heading"><div><h2 id="grants-heading">Active grants</h2><p>Revoking a grant stops future attempts from receiving it and revokes its leases. A running actor may still hold a delivered credential until stopped.</p></div><span className="fetched-time">{data.grants.length} active</span></div>
        <DataTable table={grants} label="Active grants" empty="No active grants." />
      </section>
    </> : null}

    <dialog ref={dialog} className="review-confirm" aria-labelledby="admin-confirm-title"
      onCancel={(event) => { if (busy) event.preventDefault(); }}
      onClose={() => { if (!busy) { setPending(null); setActionError(""); } }}>
      {pending ? <>
        <h3 id="admin-confirm-title">{pending.kind === "stop" ? "Stop attempt" : pending.kind === "halt" ? "Halt run" : "Revoke grant"}</h3>
        {pending.kind === "stop" ? <>
          <p>Ask the agent on <strong>{pending.attempt.stage}</strong> of <strong>{pending.attempt.projectName} / {pending.attempt.runLaunchKey}</strong> to halt at its next safe point. The stage then fails and the run continues its normal failure path. This is recorded in the audit log.</p>
          <div className="review-feedback-input"><label htmlFor="admin-stop-reason">Reason</label>
            <textarea id="admin-stop-reason" value={reason} maxLength={1000} onChange={(event) => setReason(event.target.value)} /></div>
        </> : null}
        {pending.kind === "halt" ? <p>Halt <strong>{pending.label}</strong>? Pending and escalated stages are cancelled now, running workers are stopped by the platform, and the run ends as cancelled. This cannot be undone and is recorded in the audit log.</p> : null}
        {pending.kind === "revoke" ? <p>Revoke the <strong>{pending.grant.capability}</strong> grant on <strong className="admin-wrap">{pending.grant.resource}</strong> for <strong>{pending.grant.projectName}</strong>? Future attempts lose it and its leases are revoked. Provider-side key rotation is not performed here.</p> : null}
        {actionError ? <p className="auth-alert" role="alert">{actionError}</p> : null}
        <div className="review-confirm-actions">
          <button type="button" className="secondary-button" disabled={busy} onClick={() => setPending(null)}>Cancel</button>
          <button type="button" className="primary-button danger-button" disabled={busy || (pending.kind === "stop" && !reason.trim())} onClick={() => act.mutate(pending)}>
            {busy ? "Working…" : pending.kind === "stop" ? "Stop attempt" : pending.kind === "halt" ? "Halt run" : "Revoke grant"}</button>
        </div>
      </> : null}
    </dialog>
  </PageShell>;
}
