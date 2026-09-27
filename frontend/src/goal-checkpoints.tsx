import { useRef, useState } from "react";
import { useInfiniteQuery, useMutation, useQuery } from "@tanstack/react-query";
import { createClient, ConnectError } from "@connectrpc/connect";
import { Link } from "@tanstack/react-router";
import { browserTransport, csrfToken } from "./auth";
import { WorkflowService, type Goal, type GoalRun } from "./gen/blaxsmith/api/v1/workflow_pb";
import { getCurrentReview } from "./workflow";
import { useScope } from "./workspace-ui";
const client = createClient(WorkflowService, browserTransport);

export function useGoalCheckpoints(scope: string, goalId: string) {
 return useInfiniteQuery({ queryKey: ["goal-checkpoints", scope, goalId], initialPageParam: 0n, queryFn: ({ pageParam, signal }) => client.listGoalCheckpoints({ goalId: goalId, beforeSequence: pageParam }, { signal }), getNextPageParam: (last) => last.nextBeforeSequence || undefined, refetchInterval: 10_000 });
}

export function GoalCheckpoints({ goal, runs, scope, mayEdit }: { goal: Goal; runs: GoalRun[]; scope: string; mayEdit: boolean }) {
 const { role, session } = useScope();
 const mayRecord = mayEdit && (goal.createdBy === session.data?.principalId || role === "owner" || role === "admin");
 const query = useGoalCheckpoints(scope, goal.id);
 const [runId, setRun] = useState(""); const [title, setTitle] = useState("");
 const review = useQuery({ queryKey: ["checkpoint-review", scope, runId], queryFn: ({ signal }) => getCurrentReview(runId, signal), enabled: !!runId, refetchInterval: 10_000 });
 const accepted = runs.some((run) => run.id === runId && run.state === "succeeded") && review.data && (review.data.acceptanceMode === "policy" || review.data.decision?.action === "approve");
 const retry = useRef<{ signature: string; key: string } | undefined>(undefined);
 const save = useMutation({ mutationFn: async () => {
  if (!accepted || !review.data) throw new Error("The run needs an accepted package");
  const input = { goalId: goal.id, runId, packageId: review.data.id, title: title.trim(), expectedGoalRevision: goal.revision };
  const signature = JSON.stringify({ ...input, expectedGoalRevision: goal.revision.toString() });
  if (retry.current?.signature !== signature) retry.current = { signature, key: crypto.randomUUID() };
  return client.recordGoalCheckpoint({ ...input, requestKey: retry.current.key }, { headers: { "X-Blaxsmith-CSRF": await csrfToken() } });
 }, onSuccess: async () => { retry.current = undefined; setRun(""); setTitle(""); await query.refetch(); } });
 const checkpoints = query.data?.pages.flatMap((p) => p.checkpoints) ?? [];
 return <section className="editor-card" aria-labelledby="goal-checkpoints-heading"><h2 id="goal-checkpoints-heading">Accepted checkpoints</h2>
 <p>Preserve a named milestone under its original run, plan and acceptance package. Later failures or plan changes keep this history. A checkpoint does not certify individual requirements or start another run.</p>
 {query.isError ? <p role="alert">Checkpoints could not refresh. <button className="text-action" type="button" onClick={() => void query.refetch()}>Retry</button></p> : null}
 {query.isPending ? <p role="status">Loading checkpoints…</p> : null}
 {!query.isPending && !checkpoints.length ? <p>No accepted checkpoints recorded.</p> : null}
 <ol className="event-list">{checkpoints.map((c) => <li key={c.id}><div><strong>{c.title}</strong><p>Plan {c.planVersion.toString()} · goal context {c.goalRevision.toString()} · {c.currentAcceptance ? "Acceptance remains current" : "Historical acceptance · superseded"}</p><p>Candidate <code>{c.candidateRevision}</code></p><Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: goal.projectId, runId: c.runId }}>Inspect checkpoint run</Link><time dateTime={c.createdAt}>{new Date(c.createdAt).toLocaleString()}</time></div></li>)}</ol>
 {query.hasNextPage ? <button className="secondary-button" type="button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>Load earlier checkpoints</button> : null}
 {mayRecord ? <details className="observation-provenance"><summary>Record an accepted checkpoint</summary><form className="editor-form" onSubmit={(e) => { e.preventDefault(); save.mutate(); }}><fieldset disabled={save.isPending}><legend className="sr-only">Checkpoint details</legend>
 <label className="form-field"><span>Checkpoint run</span><select value={runId} onChange={(e) => { setRun(e.target.value); save.reset(); }} required><option value="">Select a completed implementation run</option>{runs.filter((r) => r.planVersion > 0n && r.state === "succeeded").map((r) => <option key={r.id} value={r.id}>{r.id.slice(0,8)} · plan {r.planVersion.toString()}</option>)}</select></label>
 <label className="form-field"><span>Checkpoint title</span><input value={title} onChange={(e) => setTitle(e.target.value)} maxLength={300} required /></label>
 {runId && !review.isPending && !review.isError && !accepted ? <p role="status">This run has no current accepted package. Inspect its review before recording a checkpoint.</p> : null}
 {review.isError ? <p role="alert">The acceptance package could not refresh.</p> : null}
 {accepted && review.data ? <p>Accepted candidate: <code>{review.data.integratedCommit}</code>. The server rechecks this exact package when saving.</p> : null}
 <button className="primary-button" disabled={!accepted || !title.trim() || review.isError}>{save.isPending ? "Recording…" : "Record checkpoint"}</button></fieldset></form>
 {save.isError ? <p role="alert">{ConnectError.from(save.error).rawMessage}. Your draft is preserved; refresh the goal and inspect the current review before retrying.</p> : null}
 {save.isSuccess ? <p role="status">Accepted checkpoint recorded.</p> : null}
 </details> : null}
 </section>;
}
