import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, KeyRound, Plus, Settings } from "lucide-react";
import { HealthLine, useConnections } from "../connection-ui";
import { providerLabel } from "../connections";
import { DashboardLayout } from "../layouts";
import { checklistProgress, doneLabel, useProjectSetupItems } from "../setup-checklist";
import { Slot } from "../slots";
import type { TableView } from "../table-state";
import { isMissing, NotFoundPage } from "../page";
import { Card, CopyValue, EmptyState, ShowMore, StatePanel, StatTile, TimeText, Timestamp } from "../ui";
import { StatusPill, useAttentionTitle } from "../work-log";
import { asStatus, InboxLink, KindMark, RunStateBadge, useScope } from "../workspace-ui";
import { inboxKey, kindLabel, listInbox, listWorkspaceRuns, workspaceRunsKey } from "../workspace";
import {
  getLaunchAvailability, getProject, getProjectSource, getProjectVerification, launchAvailabilityQueryKey,
  projectSourceQueryKey, projectVerificationQueryKey,
} from "../workflow";

export const Route = createFileRoute("/projects/$projectId/")({ component: ProjectOverview });

const firstPage = (size: number): TableView => ({ q: "", sort: [{ id: "created", desc: true }], page: 1, size, filters: {} });

function ProjectOverview() {
  const { projectId } = Route.useParams();
  const { org, scope, role } = useScope();
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const ready = Boolean(org && project.data?.project);
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const launch = useQuery({ queryKey: launchAvailabilityQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getLaunchAvailability(projectId, signal) });
  // Connections in use: project connections plus granted organization ones
  // that this project's runs draw models from. Setup lives in the checklist.
  const own = useConnections("project", projectId, ready && Boolean(project.data?.canAdminister));
  const granted = useConnections("project_available", projectId, ready);
  const runs = useQuery({ queryKey: workspaceRunsKey(scope, firstPage(6), projectId), enabled: ready && Boolean(scope), refetchInterval: 20_000,
    queryFn: ({ signal }) => listWorkspaceRuns(firstPage(6), projectId, signal) });
  const waiting = useQuery({ queryKey: inboxKey(scope, firstPage(25), true, projectId), enabled: ready && Boolean(scope), refetchInterval: 15_000,
    queryFn: ({ signal }) => listInbox(firstPage(25), true, projectId, signal) });
  useAttentionTitle(waiting.data ? waiting.data.totalCount : null);

  const p = project.data?.project;
  const base = `/projects/${projectId}`;
  const mayLaunch = Boolean(project.data?.canLaunch);
  const canLaunch = mayLaunch && launch.data?.enabled && source.data && verification.data;
  const inUse = [...(own.data ?? []), ...(granted.data ?? [])].filter((c) => c.kind !== "git" && c.state !== "revoked")
    .map((c) => ({ connection: c, models: [...new Set(c.uses.filter((u) => u.projectId === projectId).map((u) => u.model))] }))
    .filter((c) => c.models.length);
  const available = (own.data?.length ?? 0) + (granted.data?.length ?? 0);
  // The Setup tile reports the setup checklist, so the two always agree.
  const checklist = checklistProgress(useProjectSetupItems(ready ? projectId : ""));

  if (project.isPending) return <StatePanel kind="loading" title="Loading project" />;
  if (project.isError && isMissing(project.error)) return <NotFoundPage title="Project not found" back={{ to: "/projects", label: "All projects" }}>
    This project does not exist or is not in your organization.</NotFoundPage>;
  if (project.isError || !p) return <StatePanel kind="error" title="Project unavailable" retry={() => void project.refetch()}>This project could not be loaded, or it is not in your organization.</StatePanel>;

  return <DashboardLayout title={p.name} description={`${p.slug} · created ${new Date(p.createdAt).toLocaleDateString()}`}
    actions={<>
      {mayLaunch ? <Link className="primary-button" to="/projects/$projectId/goals" params={{ projectId }}><Plus size={15} aria-hidden="true" /> New goal</Link> : null}
      {canLaunch ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> New run</Link> : null}
      <Link className="secondary-button" to="/projects/$projectId/settings" params={{ projectId }}><Settings size={15} aria-hidden="true" /> Settings</Link>
    </>}
    tiles={<>
      <StatTile label="Waiting on you" value={waiting.data?.totalCount ?? "—"} tone={waiting.data?.totalCount ? "attention" : undefined} href={`/inbox?q=${encodeURIComponent(p.name)}`} />
      <StatTile label="Runs" value={runs.data?.totalCount ?? "—"} meta="All time" href={`${base}/runs`} />
      {checklist.total ? <StatTile label="Setup" value={checklist.loading ? "—" : checklist.complete ? "Ready" : checklist.label} tone={!checklist.loading && checklist.complete ? "ok" : undefined}
        meta={checklist.loading ? "Checking…" : checklist.complete ? doneLabel(checklist.total) : checklist.leftLabel}
        href={!checklist.loading && checklist.next ? checklist.next.to : undefined} /> : null}
      <StatTile label="New runs" value={launch.data ? launch.data.enabled ? "Enabled" : "Blocked" : "—"} tone={launch.data && !launch.data.enabled ? "danger" : undefined}
        meta={launch.data && !launch.data.enabled ? launch.data.reason : "Launch availability"} />
    </>}
    slot={role ? <Slot name="project.checklist" projectId={projectId} role={role} /> : null}>
    {mayLaunch && launch.data && !launch.data.enabled ? <p className="notice dash-wide" role="note"><strong>New runs are unavailable.</strong> {launch.data.reason}</p> : null}

    <Card title="Waiting in this project" className="dash-main" description="Open items you can act on, blocking first."
      actions={<Link className="text-action" to="/inbox">Inbox <ArrowRight size={13} aria-hidden="true" /></Link>}>
      {waiting.isError ? <p className="card-body" role="alert">Open items could not be loaded.</p> : null}
      {waiting.data?.items.length ? <ol className="ledger-list"><ShowMore items={waiting.data.items} initial={4} noun="more items" render={(item) =>
        <li key={`${item.kind}-${item.id}`} className={`ledger-item kind-${item.kind}`}><KindMark kind={item.kind} />
          <div className="ledger-main"><InboxLink item={item} className="row-title">{item.title || kindLabel(item.kind)}</InboxLink>
            <small>{[kindLabel(item.kind) + (item.blocking ? " · blocking" : ""), item.runLaunchKey, item.stage].filter(Boolean).join(" · ")}</small></div>
          <Timestamp value={item.createdAt} /></li>} /></ol>
        : waiting.isSuccess ? <EmptyState title="Nothing is waiting on you here" /> : null}
    </Card>

    <Card title="Connections in use" className="dash-side" description="Model connections this project's runs draw on."
      actions={<Link className="text-action" to="/projects/$projectId/connections" params={{ projectId }}>Connections <ArrowRight size={13} aria-hidden="true" /></Link>}>
      {(own.isError && project.data?.canAdminister) || granted.isError ? <p className="card-body" role="alert">Connections could not be loaded.</p> : null}
      {inUse.length ? <ul className="ledger-list">{inUse.map(({ connection: c, models }) => <li key={c.id} className="ledger-item">
        <span className="project-symbol" aria-hidden="true"><KeyRound size={14} /></span>
        <div className="ledger-main"><Link className="row-title" to="/projects/$projectId/connections/$connectionId" params={{ projectId, connectionId: c.id }}>{c.label || `${providerLabel(c.provider)} ${c.kind === "subscription" ? "subscription" : "key"}`}</Link>
          <small>{providerLabel(c.provider)} · {c.scope === "project" ? "project" : "organization, granted"} · <span className="mono">{models.join(", ")}</span></small>
          <HealthLine connection={c} compact /></div></li>)}</ul>
        : (own.isSuccess || !project.data?.canAdminister) && granted.isSuccess ? <EmptyState title="No connections in use yet">{available ? project.data?.canAdminister ? `${available} ${available === 1 ? "connection is" : "connections are"} available; choose models to use from Connections.` : `${available} ${available === 1 ? "connection is" : "connections are"} available; a project admin chooses the models runs use.` : project.data?.canAdminister ? "Add a key or ask an admin to grant a connection." : "Ask a project admin to give this project's runs model access."}</EmptyState> : null}
    </Card>

    <Card title="Recent runs" className="dash-wide" description="The latest runs in this project."
      actions={<Link className="text-action" to="/projects/$projectId/runs" params={{ projectId }}>All runs <ArrowRight size={13} aria-hidden="true" /></Link>}>
      {runs.data?.runs.length ? <ul className="run-list">{runs.data.runs.map((run) => <li key={run.id}>
        <Link className="run-list-link" to="/projects/$projectId/runs/$runId" params={{ projectId, runId: run.id }}>
          <span className="run-list-main"><strong>{run.launchKey}</strong><small>{run.sourceCommit.slice(0, 12)}</small></span>
          <StatusPill status={asStatus(run.status)} /><RunStateBadge state={run.state} /><TimeText value={run.createdAt} />
        </Link></li>)}</ul>
        : runs.isSuccess ? <EmptyState title="No runs yet" action={canLaunch ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Start the first run</Link> : undefined}>
          {canLaunch ? "Runs use the recipe, spec, and code already committed to the repository." : "Finish setup to start the first run."}</EmptyState> : null}
    </Card>
    <p className="page-footnote dash-wide">Project ID <CopyValue value={projectId} label="Project ID" /></p>
  </DashboardLayout>;
}
