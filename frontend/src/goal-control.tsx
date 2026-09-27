import { useRef } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ConnectError } from "@connectrpc/connect";
import { controlGoal, getGoalControl, goalControlKey } from "./goals";

export function GoalControl({ goalId, scope, mayEdit }: { goalId: string; scope: string; mayEdit: boolean }) {
 const query = useQuery({ queryKey: goalControlKey(scope, goalId), queryFn: ({ signal }) => getGoalControl(goalId, signal), refetchInterval: 5000 });
 const retry = useRef<{ action: string; version: bigint; key: string } | undefined>(undefined);
 const value = query.data?.control;
 const change = useMutation({ mutationFn: async (action: string) => {
  if (!value) throw new Error("Goal controls unavailable");
  if (retry.current?.action !== action || retry.current.version !== value.version) retry.current = { action, version: value.version, key: crypto.randomUUID() };
  return controlGoal({ goalId, action, expectedVersion: retry.current.version, requestKey: retry.current.key });
 }, onSuccess: async () => { retry.current = undefined; await query.refetch(); }, onError: async () => { await query.refetch(); } });
 return <section className="observation-provenance" aria-label="Goal controls"><h2>Goal admission · {value?.state.replaceAll("_", " ") ?? "loading"}</h2>
 {value ? <><p>Control version {value.version.toString()} · {value.activeRuns.toString()} active or queued runs · {value.stoppingRuns.toString()} awaiting termination confirmation</p>
 {value.state === "paused" ? <p role="status">Paused: no new runs or attempts will start. Admitted attempts may finish and publish evidence. Resume keeps their frozen inputs and existing allowances.</p> : null}
 {value.state === "cancel_requested" ? <p role="status">Cancellation requested. Admission is closed; waiting for run termination to be confirmed.</p> : null}
 {value.state === "cancelled" ? <p role="status">Cancellation confirmed. Completed work and evidence are retained. This goal cannot resume; start a new goal for further work.</p> : null}
 {mayEdit && (value.state === "active" || value.state === "paused") ? <div className="editor-actions"><button type="button" className="secondary-button" disabled={change.isPending || query.isError} onClick={() => change.mutate(value.state === "active" ? "pause" : "resume")}>{value.state === "active" ? "Pause new work" : "Resume goal"}</button><details><summary>Cancel goal…</summary><p>Close admission permanently and stop active runs. Completed runs, reviews and evidence are retained.</p><button type="button" className="secondary-button" disabled={change.isPending || query.isError} onClick={() => change.mutate("cancel")}>Cancel goal and stop active runs</button></details></div> : null}
 </> : null}
 {query.isError ? <p role="alert">Goal controls could not refresh. <button type="button" className="text-action" onClick={() => void query.refetch()}>Retry controls</button></p> : null}
 {change.isError ? <p role="alert">{ConnectError.from(change.error).rawMessage}</p> : null}
 </section>;
}
