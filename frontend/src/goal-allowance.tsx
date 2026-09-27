import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ConnectError } from "@connectrpc/connect";
import { getGoalAllowance, goalAllowanceKey, setGoalAllowance } from "./goals";

export function GoalAllowance({ goalId, scope, mayEdit }: { goalId: string; scope: string; mayEdit: boolean }) {
 const query = useQuery({ queryKey: goalAllowanceKey(scope, goalId), queryFn: ({ signal }) => getGoalAllowance(goalId, signal), refetchInterval: 10_000 });
 const [draft, setDraft] = useState<{ expectedVersion: bigint; maxRuns: number; maxAttempts: number; admitUntil: string; maxRepeatedCheckFailures: number; noProgressSeconds: number; resetStallWindow: boolean }>();
 const save = useMutation({ mutationFn: async () => { if (!draft) throw new Error("No allowance draft"); return setGoalAllowance({ goalId, ...draft }); }, onSuccess: async () => { setDraft(undefined); await query.refetch(); }, onError: async () => { await query.refetch(); } });
 const value = query.data?.allowance;
 const reached = value && (value.admissionClosed || (value.maxAttempts > 0 && value.attempts >= BigInt(value.maxAttempts)));
 return <details className="observation-provenance"><summary>Goal execution allowance{reached ? " · admission stopped" : ""}</summary>
 <p>Shared across every planning, implementation, review and retry attempt attached to this goal. Counts include failed and cancelled work. These are admission limits, not token or spending caps. Provider usage and subscription allowance are unknown.</p>
 {query.isError ? <p role="alert">Allowance could not refresh. <button type="button" className="text-action" onClick={() => void query.refetch()}>Retry</button></p> : null}
 {value ? <><dl className="launch-prerequisites"><div><dt>Runs admitted</dt><dd>{value.runs.toString()} / {value.maxRuns || "No goal cap"}</dd></div><div><dt>Attempts admitted</dt><dd>{value.attempts.toString()} / {value.maxAttempts || "No goal cap"}</dd></div><div><dt>Admit until</dt><dd>{value.admitUntil ? new Date(value.admitUntil).toLocaleString() : "No deadline"}</dd></div></dl>
 <p>Repeated required-check failures: {value.repeatedCheckFailures >= 101n ? "≥101" : value.repeatedCheckFailures.toString()} / {value.maxRepeatedCheckFailures || "No stall cap"}. Progress window: {value.noProgressSeconds ? `${value.noProgressSeconds} seconds` : "Off"}.{value.lastProgressAt ? ` Last progress or first run: ${new Date(value.lastProgressAt).toLocaleString()}.` : ""}</p>
 {value.stallReason ? <div role="status"><strong>{value.stallReason}</strong><p>Inspect failed checks and their evidence, narrow the next plan, revise the approach or model, or wait for infrastructure and request help. The goal owner or an administrator can adjust these limits for another bounded attempt. Prior failures and usage remain recorded; no account or required check changes automatically.</p></div> : null}
 {value.stallResetAt ? <p>Recovery window started {new Date(value.stallResetAt).toLocaleString()}. Earlier evidence and all run/attempt usage remain in history.</p> : null}
 <p>Allowance version {value.version.toString()}{value.updatedAt ? ` · updated ${new Date(value.updatedAt).toLocaleString()}` : " · not configured"}</p>
 {value.maxRuns > 0 && value.runs >= BigInt(value.maxRuns) ? <p role="status">Run allowance reached. Existing runs may continue within the attempt limit and deadline.</p> : null}
 {reached ? <p role="status">New attempts are stopped by this allowance. Already admitted workers continue within their run limits; use run controls to stop them.</p> : null}
 {mayEdit && !draft ? <button type="button" className="secondary-button" onClick={() => { save.reset(); setDraft({ expectedVersion: value.version, maxRuns: value.maxRuns, maxAttempts: value.maxAttempts, admitUntil: value.admitUntil, maxRepeatedCheckFailures: value.maxRepeatedCheckFailures, noProgressSeconds: value.noProgressSeconds, resetStallWindow: false }); }}>Edit allowance</button> : null}
 </> : <p role="status">Loading allowance…</p>}
 {draft ? <form className="editor-form" onSubmit={(e) => { e.preventDefault(); save.mutate(); }}><fieldset disabled={save.isPending}><legend>Admission limits</legend>
 <label className="form-field"><span>Maximum goal runs</span><input required type="number" min={0} max={10000} value={draft.maxRuns} onChange={(e) => setDraft({ ...draft, maxRuns: Number(e.target.value) })} /></label>
 <label className="form-field"><span>Maximum goal attempts</span><input required type="number" min={0} max={100000} value={draft.maxAttempts} onChange={(e) => setDraft({ ...draft, maxAttempts: Number(e.target.value) })} /></label>
 <label className="form-field"><span>Repeated required-check failure limit</span><input required type="number" min={0} max={100} value={draft.maxRepeatedCheckFailures} onChange={(e) => setDraft({ ...draft, maxRepeatedCheckFailures: Number(e.target.value) })} /></label>
 <label className="form-field"><span>No-progress limit (seconds)</span><input required type="number" min={0} max={2592000} value={draft.noProgressSeconds} onChange={(e) => setDraft({ ...draft, noProgressSeconds: Number(e.target.value) })} /></label>
 <p className="form-hint">Zero disables each stall limit. A progress window must be at least 60 seconds. Only new passing required-check evidence or newly accepted code resets it; logs and repeated passes on the same candidate do not. Failure matching uses the exact check, policy, verdict, exit, summary and output. Active workers retain their existing time limits.</p>
 <label className="checkbox-field"><input type="checkbox" checked={draft.resetStallWindow} onChange={(e) => setDraft({ ...draft, resetStallWindow: e.target.checked })} />Start a new stall recovery window</label>
 <p className="form-hint">Explicitly permits another bounded approach under these stall thresholds. It resets only the stall observation window; all run/attempt counts, usage, deadlines and frozen checks stay in force.</p>
 <label className="form-field"><span>Admission deadline (RFC 3339)</span><input value={draft.admitUntil} placeholder="2026-10-01T18:00:00Z" onChange={(e) => setDraft({ ...draft, admitUntil: e.target.value })} /></label>
 <p>Zero removes that goal cap. An empty deadline removes the deadline. Lower limits affect new admissions immediately and preserve existing work. Each stage also retains its own runtime/retry limits.</p>
 {value && value.version !== draft.expectedVersion ? <p role="alert">The allowance changed while you were editing. Your draft is preserved; cancel and reopen to reconcile.</p> : null}
 <div className="editor-actions"><button type="button" className="secondary-button" onClick={() => { setDraft(undefined); save.reset(); }}>Cancel allowance edit</button><button className="primary-button" disabled={value?.version !== draft.expectedVersion}>Save allowance</button></div>
 </fieldset></form> : null}
 {save.isError ? <p role="alert">{ConnectError.from(save.error).rawMessage}</p> : null}
 </details>;
}
