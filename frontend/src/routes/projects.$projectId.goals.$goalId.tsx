import { UsageSummary } from "../usage-summary";
import { GoalControl } from "../goal-control";
import { GoalAllowance } from "../goal-allowance";
import { useRef, useState } from "react";
import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { createFileRoute, Link } from "@tanstack/react-router";
import { Code, ConnectError } from "@connectrpc/connect";
import { ArrowLeft, CheckCircle2, FileText, MessageSquare, Send } from "lucide-react";
import { PageHeader, PageShell } from "../page";
import { Markdown } from "../markdown";
import { useScope } from "../workspace-ui";
import { GoalPlans } from "../goal-plans";
import { QuestionForm } from "../question-form";
import type { Interaction } from "../gen/blaxsmith/api/v1/workflow_pb";
import { getGoal, goalKey, goalsKey, replyGoal } from "../goals";

export const Route = createFileRoute("/projects/$projectId/goals/$goalId")({ component: GoalWorkspace });
function GoalWorkspace() {
 const { projectId, goalId } = Route.useParams(); const { scope, role, session } = useScope(); const cache = useQueryClient();
 const query = useInfiniteQuery({ queryKey: goalKey(scope, goalId), enabled: Boolean(scope), initialPageParam: 0n,
  queryFn: ({ pageParam, signal }) => getGoal(goalId, pageParam, signal), getNextPageParam: (last) => last.nextBeforeSequence || undefined,
  refetchOnWindowFocus: false });
 const goal = query.data?.pages[0].goal;
 const [text, setText] = useState(""); const [selected, setSelected] = useState(""); const [error, setError] = useState("");
 const retry = useRef<{ signature: string; key: string } | undefined>(undefined);
 const mayEdit = ["owner", "admin", "member"].includes(role || "");
 const mutation = useMutation({ mutationFn: async (input: { kind: string; questionId?: string; optionIds?: string[]; text?: string }) => {
  if (!goal) throw new Error("Goal unavailable");
  const signature = JSON.stringify(input);
  if (retry.current?.signature !== signature) retry.current = { signature, key: crypto.randomUUID() };
  await replyGoal({ ...input, goalId, expectedRevision: goal.revision, requestKey: retry.current.key });
 }, onSuccess: async () => {
  retry.current = undefined; setError("");
  await Promise.all([cache.invalidateQueries({ queryKey: goalKey(scope, goalId) }), cache.invalidateQueries({ queryKey: goalsKey(scope, projectId) })]);
 }, onError: (cause) => {
  const code = ConnectError.from(cause).code;
  setError(code === Code.FailedPrecondition ? "Someone updated this goal. Review the latest decisions before trying again. Your draft is preserved." : code === Code.PermissionDenied ? "Your role cannot change this goal." : "Could not save. Your draft is preserved; try again.");
  if (code === Code.FailedPrecondition) void query.refetch();
 } });
 if (query.isPending) return <PageShell><p role="status">Loading goal…</p></PageShell>;
 if (!goal || goal.projectId !== projectId) return <PageShell><h1>Goal unavailable</h1><p role="alert">The goal could not be loaded in this project.</p><button className="secondary-button" onClick={() => void query.refetch()}>Try again</button></PageShell>;
 const active = goal.questions.find((q) => q.id === selected) ?? goal.questions[0];
 const answered = goal.questions.filter((q) => q.state === "answered").length;
 const entries = [...(query.data?.pages ?? [])].reverse().flatMap((page) => page.entries);
 return <PageShell><PageHeader eyebrow={`${goal.factoryId} / Goal workspace`} title={goal.title} description="A shared brief, interactive planning, and versioned artifacts." />
  <div className="goal-question-actions"><Link className="text-action" to="/projects/$projectId/goals" params={{ projectId }}><ArrowLeft size={15} aria-hidden="true" /> All goals</Link><a className="text-action" href="#goal-interview-heading">Jump to questions</a></div>
  <div className="goal-phases" aria-label="Factory planning phases"><span><MessageSquare size={17} aria-hidden="true" /> Understand</span><span>Plan</span><span>Implement</span><span>Validate</span><span>Deliver</span></div>
  {query.isError ? <p className="auth-alert" role="alert">Could not refresh this goal. Showing the last loaded version. <button type="button" className="text-action" onClick={() => void query.refetch()}>Try again</button></p> : null}
  {error ? <p className="auth-alert" role="alert">{error}</p> : null}
  <div className="goal-workspace">
   <section className="goal-conversation" aria-labelledby="goal-conversation-heading"><h2 id="goal-conversation-heading"><MessageSquare size={19} aria-hidden="true" /> Conversation</h2>
    <article className="goal-message goal-brief"><header><FileText size={15} aria-hidden="true" /><strong>Original brief</strong></header><Markdown className="md" text={goal.brief} /></article>
    {query.hasNextPage ? <button type="button" className="secondary-button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>{query.isFetchingNextPage ? "Loading…" : "Load earlier messages"}</button> : null}
    {entries.map((entry) => {
     const question = goal.questions.find((q) => q.id === entry.questionId);
     return <article className={`goal-message goal-message-${entry.kind}`} key={entry.sequence.toString()}><header><strong>{entry.kind === "message" ? "Context added" : entry.kind === "deferred" ? "Question deferred" : "Decision recorded"}</strong><span>{entry.principalId === session.data?.principalId ? "You" : `Member ${entry.principalId.slice(0, 8)}`}</span><time dateTime={entry.createdAt}>{new Date(entry.createdAt).toLocaleString()}</time></header>
      {question ? <p><strong>{question.title}</strong></p> : null}
      {entry.optionIds.length ? <p>{entry.optionIds.map((id) => question?.options.find((o) => o.id === id)?.label ?? id).join(", ")}</p> : null}
      {entry.text ? <Markdown className="md" text={entry.text} /> : null}
     </article>;
    })}
    {!entries.length ? <p className="form-hint">Add examples, constraints, or clarifications here. Starter questions are available alongside the conversation.</p> : null}
    {mayEdit ? <form className="goal-composer" onSubmit={async (e) => { e.preventDefault(); if (!text.trim() || mutation.isPending) return; try { await mutation.mutateAsync({ kind: "message", text: text.trim() }); setText(""); } catch { /* Mutation reports the error; retain the draft. */ } }}>
     <label className="form-field"><span>Add context</span><textarea rows={3} maxLength={4000} placeholder="Add an example, constraint, or a change in direction…" value={text} onChange={(e) => setText(e.target.value)} disabled={mutation.isPending} /></label>
     <div className="editor-actions"><small>Saved context for planning. This does not steer a running worker.</small><button type="submit" className="primary-button" disabled={!text.trim() || mutation.isPending}><Send size={15} aria-hidden="true" />{mutation.isPending ? "Saving…" : "Save message"}</button></div>
    </form> : <p className="form-hint">Your role can read this goal. Members, admins, and owners can contribute.</p>}
   </section>
   <aside className="goal-interview" aria-labelledby="goal-interview-heading"><h2 id="goal-interview-heading"><CheckCircle2 size={19} aria-hidden="true" /> Shape the work</h2><p>{answered} of {goal.questions.length} answered · starter questions</p>
    <nav className="goal-question-nav" aria-label="Interview questions">{goal.questions.map((q) => <button key={q.id} type="button" aria-pressed={active?.id === q.id} onClick={() => setSelected(q.id)} disabled={mutation.isPending}><span>{q.title}</span><small>{q.state === "open" ? q.blocking ? "Required" : "Optional" : q.state}</small></button>)}</nav>
    {active ? <GoalQuestion key={active.id} item={active} mayAnswer={mayEdit && !mutation.isPending} onAnswer={async (reply) => { await mutation.mutateAsync({ kind: "answer", questionId: active.id, ...reply }); }} onDefer={async () => { await mutation.mutateAsync({ kind: "deferred", questionId: active.id }); }} /> : null}
    <p className="form-hint">Starter answers guide the planner. It can ask focused follow-up questions during a run. These answers do not change project checks.</p>
    <details className="goal-provenance"><summary>Saved workspace details</summary><p>Factory {goal.factoryId} {goal.factoryVersion}<br />Revision {goal.revision.toString()}<br />Updated {new Date(goal.updatedAt).toLocaleString()}</p></details>
   </aside>
  </div>
  <GoalControl goalId={goal.id} scope={scope} mayEdit={mayEdit && (goal.createdBy === session.data?.principalId || session.data?.role === "owner" || session.data?.role === "admin")} />
  <UsageSummary scope={scope} goalId={goal.id} />
  <GoalAllowance goalId={goal.id} scope={scope} mayEdit={mayEdit && (goal.createdBy === session.data?.principalId || session.data?.role === "owner" || session.data?.role === "admin")} />
  <GoalPlans key={goal.id} goal={goal} scope={scope} mayEdit={mayEdit} contextSources={[...entries.filter((e) => e.kind === "message").map((e) => `message:${e.sequence}`), ...goal.questions.filter((q) => q.state === "answered").map((q) => `question:${q.id}`)]} />
 </PageShell>;
}
function GoalQuestion({ item, mayAnswer, onAnswer, onDefer }: { item: Interaction; mayAnswer: boolean; onAnswer: (reply: { optionIds: string[]; text: string }) => Promise<void>; onDefer: () => Promise<void> }) {
 const [editing, setEditing] = useState(item.state === "open");
 const open = editing;
 return <div><QuestionForm key={editing ? "edit" : "view"} item={open ? { ...item, state: "open" } : item} mayAnswer={mayAnswer} onAnswer={async (reply) => { await onAnswer(reply); setEditing(false); }} />
  {mayAnswer ? <div className="goal-question-actions">
   {!open ? <button type="button" className="secondary-button" onClick={() => setEditing(true)}>{item.state === "answered" ? "Revise answer" : "Answer now"}</button> : null}
   {editing && item.state !== "open" ? <button type="button" className="text-action" onClick={() => setEditing(false)}>Cancel revision</button> : null}
   {open && !item.blocking ? <button type="button" className="text-action" onClick={async () => { try { await onDefer(); setEditing(false); } catch { /* Parent displays the save failure. */ } }}>Decide later</button> : null}
  </div> : null}
 </div>;
}
