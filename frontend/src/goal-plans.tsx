import { GoalCheckpoints } from "./goal-checkpoints";
import { useEffect, useRef, useState } from "react";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ConnectError } from "@connectrpc/connect";
import type { Goal, GoalPlan } from "./gen/blaxsmith/api/v1/workflow_pb";
import { getGoalPlans, goalKey, goalPlansKey, saveGoalPlan, startGoalPlanning } from "./goals";
import { answerInteraction, listInteractions } from "./run-control";
import { listRunEvidence } from "./workflow";
import { GoalExecution } from "./goal-execution";
import { defaultGoalRunSettings, GoalRunSettings } from "./goal-run-settings";
import { QuestionForm } from "./question-form";
import { Markdown } from "./markdown";
import { PlanEditor, PlanCoverage, PlanReadiness, PlanDiff, PlanImpact, type PlanDocument } from "./plan-editor";

const activeRun = (state: string) => !["succeeded", "failed", "cancelled"].includes(state);
export function GoalPlans({ goal, scope, mayEdit, contextSources = [] }: { goal: Goal; scope: string; mayEdit: boolean; contextSources?: string[] }) {
 const native = goal.factoryId === "anvil";
 const cache = useQueryClient();
 const query = useInfiniteQuery({ queryKey: goalPlansKey(scope, goal.id), initialPageParam: 0n,
  queryFn: ({ pageParam, signal }) => getGoalPlans(goal.id, pageParam, signal), getNextPageParam: (last) => last.nextBeforeVersion || undefined,
  refetchInterval: (q) => q.state.data?.pages[0]?.runs.some((r) => activeRun(r.state)) ? 5000 : false });
 const plans = query.data?.pages.flatMap((p) => p.plans) ?? []; const allRuns = query.data?.pages[0]?.runs ?? []; const runs = allRuns.filter((r) => r.planVersion === 0n);
 const [selection, setSelection] = useState(""); const selected = plans.find((p) => p.version.toString() === selection) ?? plans[0];
 const previous = selected ? plans.find((p) => p.version === selected.version - 1n) : undefined;
 const [draft, setDraft] = useState<{ text: string; version: bigint; revision: bigint; visual: boolean }>();
 const [error, setError] = useState(""); const [notice, setNotice] = useState("");
 const retry = useRef<{ signature: string; key: string } | undefined>(undefined);
 const save = useMutation({ mutationFn: async (input: { contentJson?: string; evidenceId?: string }) => {
  const version = input.contentJson !== undefined ? draft!.version : plans[0]?.version ?? 0n;
  const revision = input.contentJson !== undefined ? draft!.revision : goal.revision;
  const signature = JSON.stringify([input, version.toString(), revision.toString()]);
  if (retry.current?.signature !== signature) retry.current = { signature, key: crypto.randomUUID() };
  return saveGoalPlan({ ...input, goalId: goal.id, expectedGoalRevision: revision, expectedPlanVersion: version, requestKey: retry.current.key });
 }, onSuccess: async (result) => { retry.current = undefined; setDraft(undefined); setError(""); setSelection(result.version.toString()); setNotice(`Plan version ${result.version} saved.`); await cache.invalidateQueries({ queryKey: goalPlansKey(scope, goal.id) }); },
 onError: (cause) => { setError(`${ConnectError.from(cause).rawMessage}. Your draft is preserved. Refresh and review the latest goal and plan before retrying.`); void query.refetch(); void cache.invalidateQueries({ queryKey: goalKey(scope, goal.id) }); } });
 const [selectedRun, setSelectedRun] = useState("");
 const run = runs.find((r) => r.id === selectedRun) ?? runs[0];
 return <section className="editor-card goal-plans" aria-labelledby="goal-plan-heading">
  <div className="editor-card-heading"><div><h2 id="goal-plan-heading">Plan the work</h2><p>Inspect the repository, resolve open questions, and turn the brief into bounded assignments. Saved plans are proposals; they do not start implementation.</p></div></div>
  {mayEdit && native ? <PlannerLaunch goal={goal} scope={scope} busy={runs.some((r) => activeRun(r.state))} onStarted={async () => { await query.refetch(); }} /> : null}
  {query.isPending ? <p role="status">Loading planning workspace…</p> : null}
  {query.isError ? <p role="alert">Could not load plans. <button className="text-action" onClick={() => void query.refetch()}>Try again</button></p> : null}
  {runs.length ? <><label className="form-field"><span>Planning run</span><select value={run?.id ?? ""} onChange={(e) => setSelectedRun(e.target.value)}>{runs.map((r) => <option key={r.id} value={r.id}>{r.id.slice(0, 8)} · {r.state} · goal revision {r.goalRevision.toString()}</option>)}</select></label>
   {run ? <div className="goal-planning-run"><p><Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: goal.projectId, runId: run.id }}>Open run, controls and checks</Link> · {run.state}{run.goalRevision !== goal.revision ? " · Based on earlier goal context" : ""}</p>
    <PlannerOutput artifactKey={native ? "anvil-plan" : "factory-plan"} key={run.id} runId={run.id} scope={scope} active={activeRun(run.state)} mayEdit={mayEdit} mayImport={run.goalRevision === goal.revision && !save.isPending && !draft} onImport={(evidenceId) => save.mutate({ evidenceId })} /></div> : null}</> : null}
  {error ? <p className="auth-alert" role="alert">{error}</p> : null}{notice ? <p role="status">{notice}</p> : null}
  {selected ? <><label className="form-field"><span>Plan version</span><select disabled={!!draft} value={selected.version.toString()} onChange={(e) => setSelection(e.target.value)}>{plans.map((p) => <option key={p.version.toString()} value={p.version.toString()}>Version {p.version.toString()} · goal revision {p.goalRevision.toString()}</option>)}</select></label>
   <p>{new Date(selected.createdAt).toLocaleString()}{selected.goalRevision !== goal.revision ? " · Earlier context — review against the current goal before proceeding." : " · Current goal context"}</p>{native ? <PlanArtifact plan={selected} /> : <article className="plan-diff"><h3>{goal.factoryId} plan</h3><p>The originating factory owns plan semantics and validation. This saved document has not been interpreted or checked by Anvil.</p><pre>{JSON.stringify(JSON.parse(selected.contentJson), null, 2)}</pre><p className="mono">SHA-256 {selected.sha256}</p></article>}
   {native && previous ? <details key={selected.version.toString()} className="observation-provenance"><summary>Compare version {previous.version.toString()} → {selected.version.toString()}</summary><PlanDiff before={JSON.parse(previous.contentJson)} after={JSON.parse(selected.contentJson)} /></details> : native && selected.version > 1n ? <p>Load earlier versions to compare this plan with its predecessor.</p> : null}
  </> : <p className="form-hint">No saved plan yet. Import a plan from your factory, or start the Anvil planner for an Anvil goal.</p>}
  {query.hasNextPage ? <button className="secondary-button" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>Load earlier versions</button> : null}
  {mayEdit && !draft ? <div className="editor-actions">{selected && native ? <button className="primary-button" disabled={!query.isSuccess || save.isPending || selected.version !== plans[0]?.version} onClick={() => setDraft({ text: selected.contentJson, version: selected.version, revision: goal.revision, visual: true })}>Edit plan</button> : null}
   <button className="secondary-button" disabled={!query.isSuccess || save.isPending || (selected && selected.version !== plans[0]?.version)} onClick={() => setDraft({ text: selected?.contentJson ? JSON.stringify(JSON.parse(selected.contentJson), null, 2) : "", version: plans[0]?.version ?? 0n, revision: goal.revision, visual: false })}>{selected ? "Revise plan JSON" : "Import plan JSON"}</button></div> : null}
  {selected && selected.version !== plans[0]?.version ? <p className="form-hint">Earlier versions are read-only. Select the latest version to revise the plan.</p> : null}
  {draft ? <form className="editor-form" onSubmit={(e) => { e.preventDefault(); save.mutate({ contentJson: draft.text }); }} onInvalidCapture={(e) => { for (let node = e.target as HTMLElement | null; node; node = node.parentElement) { if (node instanceof HTMLDetailsElement) node.open = true; } }}>
   <h3>Revise plan version {draft.version.toString()}</h3><fieldset disabled={save.isPending}><legend className="sr-only">Plan draft</legend>
   {draft.visual ? <PlanEditor contextSources={contextSources} value={JSON.parse(draft.text) as PlanDocument} onChange={(value) => setDraft({ ...draft, text: JSON.stringify(value) })} /> : <label className="form-field"><span>{native ? "Anvil plan JSON" : "Factory plan JSON"}</span><textarea rows={16} value={draft.text} onChange={(e) => setDraft({ ...draft, text: e.target.value })} spellCheck={false} maxLength={262144} /></label>}
   </fieldset>
   {native && draft.visual && selected ? <><PlanImpact before={JSON.parse(selected.contentJson)} after={JSON.parse(draft.text)} />
    {allRuns.some((run) => run.planVersion === selected.version) ? <details className="observation-provenance"><summary>Runs and evidence for the edited version</summary><p>Inspect these candidates before deciding whether revised work requires a replacement run. Showing the most recent goal runs.</p><ul>{allRuns.filter((run) => run.planVersion === selected.version).map((run) => <li key={run.id}><Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: goal.projectId, runId: run.id }}>{run.id.slice(0, 8)} · {run.state} · plan {run.planVersion.toString()}</Link></li>)}</ul></details> : null}
   </> : null}
   <p className="form-hint">Saving appends a version. Requirements must cite saved context; every task needs instructions and acceptance criteria. Validation suggestions remain user tunable.</p>
   {draft.version !== (plans[0]?.version ?? 0n) || draft.revision !== goal.revision ? <p role="alert">The goal or plan changed while you were editing. Copy your draft JSON below before cancelling, then reopen the current version to reconcile it.</p> : null}
   <details className="observation-provenance"><summary>Draft JSON for copying</summary><label className="form-field"><span>Unsaved plan JSON</span><textarea readOnly rows={8} value={draft.text} spellCheck={false} /></label></details>
   <div className="editor-actions"><button type="button" className="secondary-button" disabled={save.isPending} onClick={() => setDraft(undefined)}>Cancel edit</button><button className="primary-button" disabled={save.isPending || !draft.text.trim() || draft.version !== (plans[0]?.version ?? 0n) || draft.revision !== goal.revision}>{save.isPending ? "Saving…" : "Save new version"}</button></div></form> : null}
  {!native && allRuns.some((r) => r.planVersion > 0n) ? <ul>{allRuns.filter((r) => r.planVersion > 0n).map((r) => <li key={r.id}><Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: goal.projectId, runId: r.id }}>Factory run {r.id.slice(0,8)} · plan {r.planVersion.toString()}</Link> · {r.state}</li>)}</ul> : null}
  <GoalCheckpoints goal={goal} runs={allRuns} scope={scope} mayEdit={mayEdit} />
  {native && selected && !draft ? <GoalExecution key={`${selected.version}:${goal.revision}`} goal={goal} plan={selected} runs={allRuns.filter((r) => r.planVersion > 0n)} scope={scope} mayLaunch={mayEdit} /> : null}
 </section>;
}

function PlannerLaunch({ goal, scope, busy, onStarted }: { goal: Goal; scope: string; busy: boolean; onStarted: () => Promise<void> }) {
 const [settings, setSettings] = useState(defaultGoalRunSettings);
 const retry = useRef<{ signature: string; key: string } | undefined>(undefined);
 const start = useMutation({ mutationFn: async () => {
  const input = { goalId: goal.id, expectedRevision: goal.revision, ...settings };
  const signature = JSON.stringify({ ...input, expectedRevision: goal.revision.toString() });
  if (retry.current?.signature !== signature) retry.current = { signature, key: crypto.randomUUID() };
  return startGoalPlanning({ ...input, requestKey: retry.current.key });
 }, onSuccess: async () => { retry.current = undefined; await onStarted(); } });
 return <details className="observation-provenance"><summary>Start an Anvil planning run</summary><form className="editor-form" onSubmit={(e) => { e.preventDefault(); start.mutate(); }}>
  <fieldset disabled={busy || start.isPending}><legend className="sr-only">Planner settings</legend><GoalRunSettings projectId={goal.projectId} scope={scope} value={settings} onChange={setSettings} />
   <p className="form-hint">Uses your selected model and account. Two stages: plan, then project checks. No automatic correction retries. Time limits are not token or spending caps. Model access and runtime readiness are checked before launch.</p>
   <button className="primary-button" disabled={!settings.model.trim()}>{start.isPending ? "Starting…" : "Start planning"}</button></fieldset>
   {busy ? <p role="status">A planning run is already in progress. Open its controls to pause or halt it.</p> : null}
   {start.isError ? <p className="auth-alert" role="alert">{ConnectError.from(start.error).rawMessage}</p> : null}
  </form></details>;
}

function PlannerOutput({ runId, scope, active, mayEdit, mayImport, onImport, artifactKey }: { artifactKey: string; runId: string; scope: string; active: boolean; mayEdit: boolean; mayImport: boolean; onImport: (id: string) => void }) {
 const questions = useQuery({ queryKey: ["run-interactions", scope, runId], queryFn: ({ signal }) => listInteractions(runId, signal), refetchInterval: active ? 3000 : false });
 const evidence = useInfiniteQuery({ queryKey: ["planner-evidence", scope, runId], initialPageParam: "", queryFn: ({ pageParam, signal }) => listRunEvidence(runId, pageParam, signal), getNextPageParam: (p) => p.nextAfterId || undefined, refetchInterval: active ? 5000 : false });
 // Collect the final artifact even when completion stops the polling timer.
 useEffect(() => { if (!active) { void evidence.refetch(); void questions.refetch(); } }, [active, evidence.refetch, questions.refetch]);
 return <div>{questions.isError || evidence.isError ? <p role="alert">Planner output could not refresh. <button className="text-action" onClick={() => { void questions.refetch(); void evidence.refetch(); }}>Try again</button></p> : null}
  {questions.data?.map((q) => <QuestionForm key={q.id} item={q} mayAnswer={mayEdit} onAnswer={async (reply) => { await answerInteraction(q.id, reply.optionIds, reply.text); await questions.refetch(); }} />)}
  {evidence.data?.pages.flatMap((p) => p.evidence).filter((e) => e.current && e.kind === "artifact" && e.key === artifactKey).map((e) => <div className="goal-plan-candidate" key={e.id}><strong>Planner proposal available</strong><p>Save to validate its structure and inspect it below. This does not approve implementation or certify project checks.</p>{mayEdit ? <button className="secondary-button" disabled={!mayImport} onClick={() => onImport(e.id)}>Save proposal as plan version</button> : null}</div>)}
  {evidence.hasNextPage ? <button className="text-action" onClick={() => void evidence.fetchNextPage()}>Load more planner artifacts</button> : null}
 </div>;
}

function PlanArtifact({ plan }: { plan: GoalPlan }) {
 const doc = JSON.parse(plan.contentJson) as PlanDocument;
 return <article className="goal-plan-artifact"><h3>{doc.title}</h3><Markdown text={doc.summary} /><PlanCoverage value={doc} /><PlanReadiness value={doc} />
  <div className="goal-plan-phases">{doc.phases.map((phase) => <section key={phase.id}><header><small>{phase.id}</small><h4>{phase.title}</h4><p>{phase.outcome}</p></header>
   {doc.tasks.filter((t) => t.phase === phase.id).map((task) => <details key={task.id} className="goal-plan-task"><summary><span>{task.id}</span> {task.title}</summary><p><strong>Why:</strong> {task.reason}</p><Markdown text={task.instructions} /><p>Depends on: {task.depends_on?.join(", ") || "None"} · Requirements: {task.requirement_ids.join(", ")}</p><strong>Acceptance</strong><ul>{task.acceptance.map((text, i) => <li key={i}>{text}</li>)}</ul><strong>Suggested validation</strong>{task.validation?.length ? <ul>{task.validation.map((text, i) => <li key={i}>{text}</li>)}</ul> : <p>No additional validation proposed.</p>}</details>)}
  </section>)}</div>
  <details className="observation-provenance"><summary>Requirements and examples</summary>{doc.requirements.map((r) => <div key={r.id}><h4>{r.id} · {r.description}</h4><p>Sources: {r.sources.join(", ")}</p><ul>{r.examples.map((text, i) => <li key={i}>{text}</li>)}</ul></div>)}</details>
  {([["Assumptions", doc.assumptions], ["Out of scope", doc.out_of_scope], ["Open questions", doc.open_questions]] as const).map(([title, values]) => values?.length ? <details key={title} className="observation-provenance"><summary>{title} · {values.length}</summary><ul>{values.map((text, i) => <li key={i}>{text}</li>)}</ul></details> : null)}
  <details className="observation-provenance"><summary>Artifact provenance</summary><p>Goal revision {plan.goalRevision.toString()} · plan version {plan.version.toString()}</p><p className="mono">SHA-256 {plan.sha256}</p><p>{plan.sourceRunId ? `Derived from planning run ${plan.sourceRunId}; edits retain their own version and digest.` : "User-supplied plan"}</p></details>
 </article>;
}
