import { useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { ConnectError } from "@connectrpc/connect";
import type { Goal, GoalPlan, GoalRun, PreviewRunResponse } from "./gen/blaxsmith/api/v1/workflow_pb";
import { goalKey, goalPlansKey, launchGoalExecution, previewGoalExecution } from "./goals";
import { defaultGoalRunSettings, GoalRunSettings } from "./goal-run-settings";
import { useGoalCheckpoints } from "./goal-checkpoints";
import { LaunchPreview } from "./launch-preview";

export function GoalExecution({ goal, plan, runs, scope, mayLaunch }: { goal: Goal; plan: GoalPlan; runs: GoalRun[]; scope: string; mayLaunch: boolean }) {
 const cache = useQueryClient(); const [settings, setSettings] = useState(defaultGoalRunSettings);
 const [checkpointId, setCheckpoint] = useState("");
 const checkpoints = useGoalCheckpoints(scope, goal.id);
 const checkpointOptions = checkpoints.data?.pages.flatMap((page) => page.checkpoints) ?? [];
 const [readinessMode, setReadinessMode] = useState("advisory");
 const [reviewMode, setReviewMode] = useState("off"); const [reviewer, setReviewer] = useState(defaultGoalRunSettings);
 const [acceptance, setAcceptance] = useState("manual"); const [cycles, setCycles] = useState(0);
 const [preview, setPreview] = useState<{ signature: string; result: PreviewRunResponse }>();
 const retry = useRef<{ signature: string; key: string } | undefined>(undefined);
 const { scope: path, ...profile } = settings;
 const options = { ...profile, checkpointId, goalId: goal.id, expectedGoalRevision: goal.revision, planVersion: plan.version, correctionCycles: cycles, acceptance, readinessMode, review: reviewMode === "off" ? undefined : { mode: reviewMode, connectionId: reviewer.connectionId, harness: reviewer.harness, model: reviewer.model, effort: reviewer.effort, instructionFiles: reviewer.instructionFiles, skillFiles: reviewer.skillFiles, inputsFile: reviewer.inputsFile, agentDefinition: reviewer.agentDefinition } };
 const signature = JSON.stringify({ ...options, expectedGoalRevision: goal.revision.toString(), planVersion: plan.version.toString(), path });
 const current = preview?.signature === signature ? preview.result : undefined;
 const prepare = useMutation({ mutationFn: async () => ({ signature, result: await previewGoalExecution(goal.projectId, path, options) }), onMutate: () => setPreview(undefined), onSuccess: setPreview });
 const launch = useMutation({ mutationFn: async () => {
  if (!current?.canLaunch) throw new Error("Preview the current settings before launching.");
  const request = `${signature}:${current.bundleSha256}:${current.verificationSha256}`;
  if (retry.current?.signature !== request) retry.current = { signature: request, key: `goal-exec-${crypto.randomUUID()}` };
  return launchGoalExecution(goal.projectId, retry.current.key, path, options, current);
 }, onSuccess: async () => { await cache.invalidateQueries({ queryKey: goalPlansKey(scope, goal.id) }); }, onError: () => { setPreview(undefined); void cache.invalidateQueries({ queryKey: goalKey(scope, goal.id) }); void cache.invalidateQueries({ queryKey: goalPlansKey(scope, goal.id) }); } });
 const stale = plan.goalRevision !== goal.revision;
 const busy = prepare.isPending || launch.isPending;
 const running = runs.some((r) => !["succeeded", "failed", "cancelled"].includes(r.state));
 return <section className="goal-execution" aria-labelledby="goal-execution-heading"><h3 id="goal-execution-heading">Implement plan version {plan.version.toString()}</h3>
  <p>One implementation worker follows dependency-ordered task packets, then the platform runs your selected checks. Task packets are assignments; run status and evidence show what actually happened.</p>
  {runs.length ? <ul className="goal-execution-runs">{runs.map((r) => <li key={r.id}><Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: goal.projectId, runId: r.id }}>Plan {r.planVersion.toString()} · {r.id.slice(0, 8)}</Link> · {r.state}{r.checkpointId ? <> · continued from checkpoint <code>{r.checkpointId.slice(0, 8)}</code></> : null}</li>)}</ul> : null}
  {stale ? <p role="status">This plan uses earlier goal context. Save a revised plan before implementation.</p> : null}
  {mayLaunch && !stale ? <details className="observation-provenance"><summary>Prepare implementation</summary>
   <form className="editor-form" onSubmit={(e) => { e.preventDefault(); prepare.mutate(); }}><fieldset disabled={busy || running || launch.isSuccess}><legend className="sr-only">Implementation settings</legend>
    <label className="form-field"><span>Starting code</span><select value={checkpointId} onChange={(e) => setCheckpoint(e.target.value)}><option value="">Current project source</option>{checkpointOptions.map((c) => <option key={c.id} value={c.id} disabled={!c.currentAcceptance}>{c.title} · {c.candidateRevision.slice(0, 12)}{c.currentAcceptance ? "" : " · superseded"}</option>)}</select></label>
    {checkpoints.isError ? <p role="alert">Checkpoints could not refresh. <button type="button" className="text-action" onClick={() => void checkpoints.refetch()}>Retry checkpoints</button></p> : null}
    {checkpoints.hasNextPage ? <button type="button" className="text-action" disabled={checkpoints.isFetchingNextPage} onClick={() => void checkpoints.fetchNextPage()}>Load earlier source checkpoints</button> : null}
    {checkpointId ? <p className="form-hint">Continue from the exact accepted commit. The current plan, access and project checks apply. Earlier acceptance does not certify this run or skip its tasks.</p> : null}
    <GoalRunSettings phase="implementation" projectId={goal.projectId} scope={scope} value={settings} onChange={setSettings} />
    <label className="form-field"><span>Planning readiness</span><select value={readinessMode} onChange={(e) => setReadinessMode(e.target.value)}><option value="off">Off — do not gate launch on questions</option><option value="advisory">Advisory — show unresolved questions</option><option value="required">Required — resolve blocking questions first</option></select></label>
    <p className="form-hint">Required readiness checks classified blocking unknowns and unclassified open questions. Investigable, assumed and deferred items stay visible. This does not change project verification or approve proposed decisions.</p>
    <label className="form-field"><span>Independent code review</span><select value={reviewMode} onChange={(e) => setReviewMode(e.target.value)}><option value="off">Off — no separate review</option><option value="advisory">Advisory — record findings</option><option value="required">Required — findings must be resolved</option></select></label>
    {reviewMode !== "off" ? <fieldset><legend>Independent reviewer</legend><GoalRunSettings profileOnly phase="review" projectId={goal.projectId} scope={scope} value={reviewer} onChange={setReviewer} /><p className="form-hint">A separate worker reviews the candidate after project checks. Choose its own model, effort, rules and skills. The shared repository scope and time limit apply. This consumes additional model usage.</p></fieldset> : null}
    <div className="recipe-grid"><label className="form-field"><span>Acceptance</span><select value={acceptance} onChange={(e) => setAcceptance(e.target.value)}><option value="manual">Human review</option><option value="policy">Selected checks only</option></select></label>
     <label className="form-field"><span>Correction cycles</span><input type="number" min={0} max={10} value={cycles} onChange={(e) => setCycles(Number(e.target.value))} required /></label></div>
    <p className="form-hint">Up to {cycles} automatic repairs per failing required stage{reviewMode === "required" ? ` (up to ${cycles * 2} across checks and review)` : ""}; zero stops at the first failure. Retries after interrupted attempts can add work. Each attempt has the selected time limit. These are not token or spending caps. Project check modes stay unchanged. Launch writes a candidate run branch; target-branch merge is separate.</p>
    {acceptance === "policy" ? <p className="form-hint">Policy acceptance finishes without a human approval gate once selected requirements pass. With no required checks, acceptance does not establish tested correctness.</p> : null}
    <button type="submit" className="secondary-button" disabled={!settings.model.trim() || (reviewMode !== "off" && !reviewer.model.trim())}>{prepare.isPending ? "Preparing…" : "Preview implementation"}</button>
   </fieldset></form>
   {prepare.isError ? <p className="auth-alert" role="alert">{ConnectError.from(prepare.error).rawMessage}</p> : null}
   {current ? <><LaunchPreview key={current.bundleSha256} preview={current} /><button type="button" className="primary-button" disabled={!current.canLaunch || busy || running || launch.isSuccess} onClick={() => launch.mutate()}>{launch.isPending ? "Launching…" : "Launch implementation"}</button></> : preview ? <p role="status">Settings changed. Preview again to inspect the updated assignment.</p> : null}
   {launch.isError ? <p className="auth-alert" role="alert">{ConnectError.from(launch.error).rawMessage}. Review existing runs and preview again before retrying.</p> : null}
  </details> : null}
  {running ? <p role="status">An implementation run is in progress. Open it to inspect work, questions, checks, or controls.</p> : null}
  {launch.data?.run ? <p role="status">Implementation launched. <Link className="text-action" to="/projects/$projectId/runs/$runId" params={{ projectId: goal.projectId, runId: launch.data.run.id }}>Open run</Link></p> : null}
 </section>;
}
