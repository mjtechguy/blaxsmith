import { useQuery } from "@tanstack/react-query";
import { createClient } from "@connectrpc/connect";
import { browserTransport } from "./auth";
import { WorkflowService } from "./gen/blaxsmith/api/v1/workflow_pb";
const client = createClient(WorkflowService, browserTransport);

function dollars(micros: string) {
 const n = BigInt(micros);
 return `$${(n / 1_000_000n).toLocaleString()}.${(n % 1_000_000n).toString().padStart(6, "0")}`;
}
export function UsageSummary({ scope, runId, goalId }: { scope: string; runId?: string; goalId?: string }) {
 const query = useQuery({ queryKey: ["usage", scope, runId ?? "", goalId ?? ""], queryFn: async ({ signal }) => (await (goalId ? client.getGoalUsage({ goalId }, { signal }) : client.getRunUsage({ runId }, { signal }))).usage, refetchInterval: 10_000 });
 const u = query.data;
 return <details className="observation-provenance"><summary>Reported usage</summary>
 <p>Harness-reported subtotals across {goalId ? "all goal runs and attempts" : "all run attempts"}, including retries and cancelled work. Reports are unverified. Interrupted work, human takeover and provider overhead may be missing.</p>
 {query.isError ? <p role="alert">Usage could not refresh. <button type="button" className="text-action" onClick={() => void query.refetch()}>Retry</button></p> : null}
 {u ? <>
 <p>{u.reportedAttempts.toString()} of {u.attempts.toString()} attempts have reports. Coverage does not establish complete measurement.</p>
 {u.reports > 0n ? <dl className="launch-prerequisites"><div><dt>Reported input tokens (including cache)</dt><dd>{BigInt(u.inputTokens).toLocaleString()}</dd></div><div><dt>Reported output tokens (including reasoning)</dt><dd>{BigInt(u.outputTokens).toLocaleString()}</dd></div><div><dt>Reported cost estimate subtotal</dt><dd>{u.costReports > 0n ? dollars(u.costMicrosUsd) : "Unknown"}</dd></div></dl> : <p>No harness usage reports collected. Token usage and cost are unknown, not zero.</p>}
 {u.costReports > 0n ? <p>Cost is available for {u.costReports.toString()} of {u.reports.toString()} reports. This estimate is not verified billing, subscription quota or a spending cap.</p> : null}
 </> : query.isPending ? <p role="status">Loading reported usage…</p> : null}
 </details>;
}
