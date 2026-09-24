import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ArrowRight, ChevronRight, CircleCheck, CircleDashed, ListChecks, X } from "lucide-react";
import { listAuditEvents } from "./admin";
import { currentSession, sessionQueryKey } from "./auth";
import { listConnections } from "./connections";
import { listRecipes, recipesKey } from "./recipes";
import { listMembers } from "./users";
import {
  getProjectSource, getProjectVerification, listProjectModelAccess, listProjects, listRuns, projectModelAccessQueryKey, projectSourceQueryKey,
  projectVerificationQueryKey,
} from "./workflow";

// One step: done is undefined while its data loads. to is an app path.
export type ChecklistItem = { id: string; label: string; hint: string; done: boolean | undefined; to: string };

// One summary for a checklist and any tile that reports on it, so both read the same.
export function checklistProgress(items: ChecklistItem[]) {
  const done = items.filter((item) => item.done).length;
  const left = items.length - done;
  return {
    done, total: items.length, left, loading: items.some((item) => item.done === undefined), complete: left === 0,
    next: items.find((item) => item.done === false),
    label: `${done} of ${items.length} done`, leftLabel: `${left} ${left === 1 ? "step" : "steps"} left`,
  };
}

// Dismissal is a per-user browser convenience, so localStorage is enough;
// storage can be unavailable, and then the list simply shows again.
function useDismissed(key: string): [boolean, (value: boolean) => void] {
  const [dismissed, setState] = useState(() => { try { return localStorage.getItem(key) === "1"; } catch { return false; } });
  return [dismissed, (value) => {
    setState(value);
    try { if (value) localStorage.setItem(key, "1"); else localStorage.removeItem(key); } catch { /* Storage unavailable. */ }
  }];
}

function useSession() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  return { org: session.data?.organizationId || "", principal: session.data?.principalId || "" };
}

function ChecklistStep({ item }: { item: ChecklistItem }) {
  return <li className={item.done ? "is-done" : undefined}>
    {item.done ? <CircleCheck size={17} aria-hidden="true" /> : <CircleDashed size={17} aria-hidden="true" />}
    <span><strong>{item.label}</strong><small>{item.done === undefined ? "Checking…" : item.done ? "Done" : item.hint}</small></span>
    {item.done ? null : <Link className="text-action" to={item.to as "/"}>{item.done === undefined ? "Open" : "Set up"} <ArrowRight size={13} aria-hidden="true" /></Link>}
    <span className="sr-only">{item.done ? "complete" : "incomplete"}</span>
  </li>;
}

// Remaining steps are listed; finished ones fold into one "N done · Show"
// disclosure. When everything is done a single dismissible line remains.
export function SetupChecklist({ title, items, storageKey }: { title: string; items: ChecklistItem[]; storageKey: string }) {
  const [dismissed, setDismissed] = useDismissed(storageKey);
  const [showDone, setShowDone] = useState(false);
  const { done, total, loading, complete, label, leftLabel } = checklistProgress(items);
  const headingId = `${storageKey}-heading`;
  const doneId = `${storageKey}-done`;
  const dismiss = <button type="button" className="secondary-button" onClick={() => setDismissed(true)} aria-label={`Dismiss ${title}`}><X size={15} aria-hidden="true" /> Dismiss</button>;
  if (dismissed) return complete ? null
    : <button type="button" className="text-action checklist-restore" onClick={() => setDismissed(false)}><ListChecks size={14} aria-hidden="true" /> Show setup checklist ({label})</button>;
  if (complete) return <section className="checklist-complete" aria-label={title}>
    <CircleCheck size={17} aria-hidden="true" /><p><strong>Setup complete</strong> <span>All {total} steps done.</span></p>{dismiss}
  </section>;
  const finished = items.filter((item) => item.done);
  return <section className="table-section checklist" aria-labelledby={headingId}>
    <div className="table-heading"><div><h2 id={headingId}><ListChecks size={15} aria-hidden="true" /> {title}</h2>
      <p>{loading ? "Checking what is set up…" : `${label} · ${leftLabel}. Each step links to where you do it.`}</p></div>{dismiss}</div>
    <ol className="checklist-items">{items.filter((item) => !item.done).map((item) => <ChecklistStep key={item.id} item={item} />)}</ol>
    {done ? <div className="checklist-done">
      <button type="button" className="disclosure-toggle" aria-expanded={showDone} aria-controls={doneId} onClick={() => setShowDone(!showDone)}>
        <ChevronRight size={14} aria-hidden="true" className={showDone ? "is-open" : undefined} />{done} done · {showDone ? "Hide" : "Show"}</button>
      {showDone ? <ol id={doneId} className="checklist-items" aria-label="Completed steps">{finished.map((item) => <ChecklistStep key={item.id} item={item} />)}</ol> : null}
    </div> : null}
  </section>;
}

// Organization setup for owners and admins, from data the admin pages already read.
export function OrgSetupChecklist() {
  const { org, principal } = useSession();
  const on = Boolean(org);
  const connections = useQuery({ queryKey: ["connections", org, "organization", ""], enabled: on, queryFn: ({ signal }) => listConnections("organization", "", signal) });
  const members = useQuery({ queryKey: ["org-members", org], enabled: on, queryFn: ({ signal }) => listMembers(signal) });
  const projects = useQuery({ queryKey: ["projects", org, "", "checklist"], enabled: on, queryFn: ({ signal }) => listProjects("", "", "created_at", "desc", signal) });
  const runs = useQuery({ queryKey: ["admin-audit", org, "workflow.run.launched", "", "", "checklist"], enabled: on,
    queryFn: ({ signal }) => listAuditEvents("", "workflow.run.launched", "", "", signal) });
  const active = connections.data?.filter((c) => c.state === "active");
  const items: ChecklistItem[] = [
    { id: "model", label: "Add a model connection", hint: "An organization API key that projects can be granted.", to: "/admin/connections/new/api-key",
      done: active && active.some((c) => c.kind !== "git") },
    { id: "github", label: "Connect GitHub", hint: "Lets projects pick private repositories and push run branches.", to: "/admin/connections/new/git",
      done: active && active.some((c) => c.kind === "git") },
    { id: "users", label: "Invite your team", hint: "Add members who will launch and review runs.", to: "/admin/users/new",
      done: members.data && members.data.members.length > 1 },
    { id: "project", label: "Create a project", hint: "A project holds a repository, checks, and runs.", to: "/projects/new",
      done: projects.data && projects.data.projects.length > 0 },
    { id: "run", label: "Start the first run", hint: "Open a project and start a run from a recipe.", to: projects.data?.projects[0] ? `/projects/${projects.data.projects[0].id}/runs/new` : "/",
      done: runs.data && runs.data.events.length > 0 },
  ];
  return <SetupChecklist title="Set up your organization" items={items} storageKey={`blaxsmith-checklist:${org}:${principal}:org`} />;
}

// Project setup; each step matches a prerequisite for launching runs. The
// project overview's Setup tile reads these same steps.
export function useProjectSetupItems(projectId: string): ChecklistItem[] {
  const { org } = useSession();
  const on = Boolean(org && projectId);
  const source = useQuery({ queryKey: projectSourceQueryKey(org, projectId), enabled: on, queryFn: ({ signal }) => getProjectSource(projectId, signal) });
  const verification = useQuery({ queryKey: projectVerificationQueryKey(org, projectId), enabled: on, queryFn: ({ signal }) => getProjectVerification(projectId, signal) });
  const recipes = useQuery({ queryKey: recipesKey(org, projectId), enabled: on, queryFn: ({ signal }) => listRecipes(projectId, signal) });
  const access = useQuery({ queryKey: projectModelAccessQueryKey(org, projectId), enabled: on, queryFn: ({ signal }) => listProjectModelAccess(projectId, signal) });
  const runs = useQuery({ queryKey: ["runs", org, projectId, "checklist"], enabled: on, queryFn: ({ signal }) => listRuns(projectId, "", "", "created_at", "desc", signal) });
  const settled = <T,>(q: { isSuccess: boolean; isError: boolean; data?: T }, done: (data: T) => boolean) =>
    q.isSuccess ? done(q.data as T) : q.isError ? false : undefined;
  return [
    { id: "source", label: "Connect the repository", hint: "Choose the Git repository and ref runs start from.", to: `/projects/${projectId}/settings/source`, done: settled(source, Boolean) },
    { id: "verification", label: "Set verification checks", hint: "At least one check is required to launch.", to: `/projects/${projectId}/settings/verification`, done: settled(verification, Boolean) },
    { id: "recipe", label: "Make a recipe available", hint: "Create a project recipe or ask an admin to grant one.", to: `/projects/${projectId}/recipes`,
      done: settled(recipes, (r) => r.recipes.length > 0) },
    { id: "models", label: "Give runs model access", hint: "Use a granted organization connection or add a project key.", to: `/projects/${projectId}/connections`,
      done: settled(access, (a) => a.access.length > 0) },
    { id: "run", label: "Start the first run", hint: "Pick a recipe version and committed inputs.", to: `/projects/${projectId}/runs/new`, done: settled(runs, (r) => r.runs.length > 0) },
  ];
}

export function ProjectSetupChecklist({ projectId }: { projectId: string }) {
  const { org, principal } = useSession();
  const items = useProjectSetupItems(projectId);
  return <SetupChecklist title="Set up this project" items={items} storageKey={`blaxsmith-checklist:${org}:${principal}:project:${projectId}`} />;
}
