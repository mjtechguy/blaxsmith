import { useQuery } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { ArrowRight, BookCopy, Check, GitBranch, KeyRound, ListChecks, Plus, Settings } from "lucide-react";
import { DashboardLayout } from "../layouts";
import { Slot } from "../slots";
import type { TableView } from "../table-state";
import { Card, CopyValue, EmptyState, ShowMore, StatePanel, StatTile, Timestamp } from "../ui";
import { StatusPill, useAttentionTitle } from "../work-log";
import { asStatus, InboxLink, KindMark, RunStateBadge, useScope } from "../workspace-ui";
import { inboxKey, kindLabel, listInbox, listWorkspaceRuns, workspaceRunsKey } from "../workspace";
import {
  getLaunchAvailability, getProject, getProjectSource, getProjectVerification, launchAvailabilityQueryKey, listProjectModelAccess,
  projectModelAccessQueryKey, projectSourceQueryKey, projectVerificationQueryKey,
} from "../workflow";
import { listRecipes, recipesKey } from "../recipes";

export const Route = createFileRoute("/projects/$projectId/")({ component: ProjectOverview });

const firstPage = (size: number): TableView => ({ q: "", sort: [{ id: "created", desc: true }], page: 1, size, filters: {} });

function ProjectOverview() {
  const { projectId } = Route.useParams();
  const { org, scope, role, isAdmin, isMember } = useScope();
  const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
  const ready = Boolean(org && project.data?.project);
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const modelAccess = useQuery({ queryKey: projectModelAccessQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => listProjectModelAccess(projectId, signal) });
  const launch = useQuery({ queryKey: launchAvailabilityQueryKey(org, projectId), enabled: ready, queryFn: ({ signal }) => getLaunchAvailability(projectId, signal) });
  const recipes = useQuery({ queryKey: recipesKey(org, projectId), enabled: ready, queryFn: ({ signal }) => listRecipes(projectId, signal) });
  const runs = useQuery({ queryKey: workspaceRunsKey(scope, firstPage(6), projectId), enabled: ready && Boolean(scope), refetchInterval: 20_000,
    queryFn: ({ signal }) => listWorkspaceRuns(firstPage(6), projectId, signal) });
  const waiting = useQuery({ queryKey: inboxKey(scope, firstPage(25), true, projectId), enabled: ready && Boolean(scope), refetchInterval: 15_000,
    queryFn: ({ signal }) => listInbox(firstPage(25), true, projectId, signal) });
  useAttentionTitle(waiting.data ? waiting.data.totalCount : null);

  const p = project.data?.project;
  const base = `/projects/${projectId}`;
  const canLaunch = isMember && launch.data?.enabled && source.data && verification.data;
  const setup = [
    { label: "Git source", done: Boolean(source.data), href: `${base}/settings/source`, icon: GitBranch,
      detail: source.data ? `${source.data.repositoryUrl.replace(/^https:\/\//, "")}${source.data.ref ? ` @ ${source.data.ref}` : ""}` : "Add a GitHub or GitLab repository" },
    { label: "Verification checks", done: Boolean(verification.data), href: `${base}/settings/verification`, icon: ListChecks,
      detail: verification.data ? `${verification.data.checks.length} ${verification.data.checks.length === 1 ? "check" : "checks"} · version ${verification.data.version.toString()}` : "At least one check is required to launch" },
    { label: "Model access", done: Boolean(modelAccess.data?.access.length), href: `${base}/connections`, icon: KeyRound,
      detail: modelAccess.data?.access.length ? `${modelAccess.data.access.length} model ${modelAccess.data.access.length === 1 ? "grant" : "grants"}` : "Attach a key or a granted connection" },
    { label: "Recipes", done: Boolean(recipes.data?.recipes.length), href: `${base}/recipes`, icon: BookCopy,
      detail: recipes.data ? `${recipes.data.recipes.length} available to this project` : "Loading…" },
  ];
  const doneCount = setup.filter((s) => s.done).length;

  if (project.isPending) return <StatePanel kind="loading" title="Loading project" />;
  if (project.isError || !p) return <StatePanel kind="error" title="Project unavailable" retry={() => void project.refetch()}>This project could not be loaded, or it is not in your organization.</StatePanel>;

  return <DashboardLayout title={p.name} description={`${p.slug} · created ${new Date(p.createdAt).toLocaleDateString()}`}
    actions={<>
      {canLaunch ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> New run</Link> : null}
      <Link className="secondary-button" to="/projects/$projectId/settings" params={{ projectId }}><Settings size={15} aria-hidden="true" /> Settings</Link>
    </>}
    tiles={<>
      <StatTile label="Waiting on you" value={waiting.data?.totalCount ?? "—"} tone={waiting.data?.totalCount ? "attention" : undefined} href={`/inbox?q=${encodeURIComponent(p.name)}`} />
      <StatTile label="Runs" value={runs.data?.totalCount ?? "—"} meta="All time" href={`${base}/runs`} />
      <StatTile label="Setup" value={`${doneCount} of ${setup.length}`} tone={doneCount === setup.length ? "ok" : undefined} meta={doneCount === setup.length ? "Ready" : "Steps remaining"} />
      <StatTile label="New runs" value={launch.data ? launch.data.enabled ? "Enabled" : "Blocked" : "—"} tone={launch.data && !launch.data.enabled ? "danger" : undefined}
        meta={launch.data && !launch.data.enabled ? launch.data.reason : "Launch availability"} />
    </>}
    slot={role ? <Slot name="project.checklist" projectId={projectId} role={role} /> : null}>
    {isMember && launch.data && !launch.data.enabled ? <p className="notice dash-wide" role="note"><strong>New runs are unavailable.</strong> {launch.data.reason}</p> : null}

    <Card title="Waiting in this project" className="dash-main" description="Open items you can act on, blocking first."
      actions={<Link className="text-action" to="/inbox">Inbox <ArrowRight size={13} aria-hidden="true" /></Link>}>
      {waiting.isError ? <p className="card-body" role="alert">Open items could not be loaded.</p> : null}
      {waiting.data?.items.length ? <ol className="ledger-list"><ShowMore items={waiting.data.items} initial={4} noun="more items" render={(item) =>
        <li key={`${item.kind}-${item.id}`} className={`ledger-item kind-${item.kind}`}><KindMark kind={item.kind} />
          <div className="ledger-main"><InboxLink item={item} className="row-title">{item.title || kindLabel(item.kind)}</InboxLink>
            <small>{kindLabel(item.kind)}{item.blocking ? " · blocking" : ""} · {item.runLaunchKey}{item.stage ? ` · ${item.stage}` : ""}</small></div>
          <Timestamp value={item.createdAt} /></li>} /></ol>
        : waiting.isSuccess ? <EmptyState title="Nothing is waiting on you here" /> : null}
    </Card>

    <Card title="Setup" className="dash-side" description={doneCount === setup.length ? "Everything a run needs is configured." : "What a run needs before it can launch."}>
      <ul className="setup-list">{setup.map((step) => <li key={step.label} className={step.done ? "is-done" : undefined}>
        <span className="setup-mark" aria-hidden="true">{step.done ? <Check size={13} /> : <step.icon size={13} />}</span>
        <Link to={step.href as "/"}><strong>{step.label}</strong><small>{step.detail}</small></Link>
        <span className="sr-only">{step.done ? "configured" : "not configured"}</span></li>)}</ul>
      {!isAdmin && (!source.data || !verification.data) ? <p className="card-note">Owners and admins configure the source and checks.</p> : null}
    </Card>

    <Card title="Recent runs" className="dash-wide" description="The latest runs in this project."
      actions={<Link className="text-action" to="/projects/$projectId/runs" params={{ projectId }}>All runs <ArrowRight size={13} aria-hidden="true" /></Link>}>
      {runs.data?.runs.length ? <ul className="run-list">{runs.data.runs.map((run) => <li key={run.id}>
        <Link className="run-list-link" to="/projects/$projectId/runs/$runId" params={{ projectId, runId: run.id }}>
          <span className="run-list-main"><strong>{run.launchKey}</strong><small>{run.sourceCommit.slice(0, 12)}</small></span>
          <StatusPill status={asStatus(run.status)} /><RunStateBadge state={run.state} /><time dateTime={run.createdAt}>{new Date(run.createdAt).toLocaleString()}</time>
        </Link></li>)}</ul>
        : runs.isSuccess ? <EmptyState title="No runs yet" action={canLaunch ? <Link className="primary-button" to="/projects/$projectId/runs/new" params={{ projectId }}><Plus size={15} aria-hidden="true" /> Start the first run</Link> : undefined}>
          {canLaunch ? "Runs use the recipe, spec, and code already committed to the repository." : "Finish setup to start the first run."}</EmptyState> : null}
    </Card>
    <p className="page-footnote dash-wide">Project ID <CopyValue value={projectId} label="Project ID" /></p>
  </DashboardLayout>;
}
