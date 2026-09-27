import { useState } from "react";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link, useNavigate } from "@tanstack/react-router";
import { MessageSquare, Plus } from "lucide-react";
import { Code, ConnectError } from "@connectrpc/connect";
import { PageHeader, PageShell } from "../page";
import { useScope } from "../workspace-ui";
import { getProject } from "../workflow";
import { createGoal, goalsKey, listGoals } from "../goals";

export const Route = createFileRoute("/projects/$projectId/goals/")({ component: Goals });
function Goals() {
 const { projectId } = Route.useParams();
 const { scope, org, role } = useScope();
 const navigate = useNavigate(); const cache = useQueryClient();
 const project = useQuery({ queryKey: ["project", org, projectId], enabled: Boolean(org), queryFn: ({ signal }) => getProject(projectId, signal) });
 const goals = useInfiniteQuery({ queryKey: goalsKey(scope, projectId), enabled: Boolean(scope && project.data?.project), initialPageParam: "", queryFn: ({ pageParam, signal }) => listGoals(projectId, pageParam, signal), getNextPageParam: (last) => last.nextBeforeId || undefined });
 const [title, setTitle] = useState(""); const [brief, setBrief] = useState("");
 const [key, setKey] = useState(() => crypto.randomUUID());
 const [pending, setPending] = useState(false); const [error, setError] = useState("");
 const mayEdit = ["owner", "admin", "member"].includes(role || "");
 return <PageShell><PageHeader eyebrow="Anvil / Goals" title="What should we build?" description="Start with a brief. Keep the conversation, decisions, and planning context together before launching work." />
  {project.isError ? <p className="auth-alert" role="alert">Project unavailable.</p> : null}
  {project.data?.project && mayEdit ? <section className="editor-card goal-create"><h2><MessageSquare size={20} aria-hidden="true" /> Start an Anvil goal</h2>
   <form className="editor-form" onSubmit={async (event) => {
    event.preventDefault(); if (pending) return; setPending(true); setError("");
    try { const response = await createGoal(projectId, key, title.trim(), brief.trim()); if (!response.goal) throw new Error("Missing goal"); await cache.invalidateQueries({ queryKey: goalsKey(scope, projectId) }); await navigate({ to: "/projects/$projectId/goals/$goalId", params: { projectId, goalId: response.goal.id } }); }
    catch (cause) { const code = ConnectError.from(cause).code; setError(code === Code.PermissionDenied ? "Your role cannot create goals." : code === Code.FailedPrecondition ? "This request changed. Edit the brief and submit again." : "Could not save this goal. Your brief is still here; try again."); }
    finally { setPending(false); }
   }}>
    <label className="form-field"><span>Goal title</span><input value={title} required maxLength={160} placeholder="Add passwordless sign-in" onChange={(e) => { setTitle(e.target.value); setKey(crypto.randomUUID()); }} disabled={pending} /></label>
    <label className="form-field"><span>What do you want to accomplish?</span><textarea rows={5} value={brief} required maxLength={16000} placeholder="Describe the outcome, what already exists, and anything that must stay unchanged." onChange={(e) => { setBrief(e.target.value); setKey(crypto.randomUUID()); }} disabled={pending} /></label>
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    <div className="editor-actions"><span className="form-hint">No worker starts and no model tokens are used.</span><button className="primary-button" disabled={pending || !title.trim() || !brief.trim()}><Plus size={16} aria-hidden="true" />{pending ? "Saving…" : "Start conversation"}</button></div>
   </form></section> : null}
  <section className="goal-list" aria-label="Project goals"><h2>Project goals</h2>
   {goals.isPending && project.isSuccess ? <p role="status">Loading goals…</p> : null}
   {goals.isError ? <p role="alert">Goals could not be loaded. <button className="text-action" onClick={() => void goals.refetch()}>Try again</button></p> : null}
   {goals.data?.pages.flatMap((page) => page.goals).map((goal) => <Link key={goal.id} className="goal-list-item" to="/projects/$projectId/goals/$goalId" params={{ projectId, goalId: goal.id }}><MessageSquare size={20} aria-hidden="true" /><span><strong>{goal.title}</strong><small>{goal.factoryId} · Planning · updated {new Date(goal.updatedAt).toLocaleString()}</small></span></Link>)}
   {goals.data && !goals.data.pages[0].goals.length ? <p>No goals yet. Start with the outcome you want.</p> : null}
   {goals.hasNextPage ? <button className="secondary-button" disabled={goals.isFetchingNextPage} onClick={() => void goals.fetchNextPage()}>Load more goals</button> : null}
  </section>
 </PageShell>;
}
