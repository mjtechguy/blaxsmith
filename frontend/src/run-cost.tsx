// Run page → Cost tab and stage cost chips (docs/model-gateway-plan.md §9.4).
import { useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { CollectionTable, inSet, useLocalView, type GridColumn } from "./data-table";
import { compact, getRunCost, percent, routeKindLabels, runCostKey, totalTokens, usd } from "./gateway";
import type { ModelCall, StageCost } from "./gen/blaxsmith/api/v1/gateway_pb";
import { Card, StatePanel, StatTile, Timestamp } from "./ui";

export function useRunCost(scope: string, runId: string, enabled: boolean) {
  return useQuery({ queryKey: runCostKey(scope, runId), enabled: enabled && Boolean(scope), refetchInterval: 15_000,
    queryFn: ({ signal }) => getRunCost(runId, signal) });
}

export function StageCostChip({ stage, stages }: { stage: string; stages?: StageCost[] }) {
  const total = stages?.filter((s) => s.stage === stage).reduce((sum, s) => sum + Number(s.totals?.costUsdMicros ?? 0), 0);
  if (!total) return null;
  return <span className="cost-chip" title="Estimated model cost for this stage">{usd(total)}</span>;
}

const apiLabels: Record<string, string> = { anthropic_messages: "Messages", openai_responses: "Responses", openai_chat: "Chat Completions", other: "Other" };

export function RunCostTab({ scope, runId }: { scope: string; runId: string }) {
  const cost = useRunCost(scope, runId, true);
  const [stageView, setStageView] = useLocalView({ size: 20, sort: [{ id: "cost", desc: true }] });
  const [callView, setCallView] = useLocalView({ size: 20 });
  const stageColumns = useMemo<GridColumn<StageCost>[]>(() => [
    { id: "stage", accessorKey: "stage", header: "Stage", enableHiding: false, cell: ({ row }) => <strong>{row.original.stage}</strong> },
    { id: "cost", accessorFn: (s) => Number(s.totals?.costUsdMicros ?? 0), header: "Est. cost", cell: ({ row }) => <strong className="num">{usd(row.original.totals?.costUsdMicros ?? 0n)}</strong> },
    { id: "tokens", accessorFn: (s) => totalTokens(s.totals), header: "Tokens", cell: ({ row }) => <span className="num">{compact(totalTokens(row.original.totals))}</span> },
    { id: "input", accessorFn: (s) => Number(s.totals?.inputTokens ?? 0), header: "Input", cell: ({ row }) => <span className="num">{compact(row.original.totals?.inputTokens ?? 0n)}</span> },
    { id: "output", accessorFn: (s) => Number(s.totals?.outputTokens ?? 0), header: "Output", cell: ({ row }) => <span className="num">{compact(row.original.totals?.outputTokens ?? 0n)}</span> },
    { id: "cache", accessorKey: "cacheHitRatio", header: "Cache hit", cell: ({ row }) => <span className="num">{percent(row.original.cacheHitRatio, 1)}</span> },
    { id: "requests", accessorFn: (s) => Number(s.totals?.requests ?? 0), header: "Requests", cell: ({ row }) => <span className="num">{row.original.totals?.requests.toString()}</span> },
    { id: "errors", accessorFn: (s) => Number(s.totals?.errors ?? 0), header: "Errors", cell: ({ row }) => <span className="num">{row.original.totals?.errors.toString()}</span> },
  ], []);
  const callColumns = useMemo<GridColumn<ModelCall>[]>(() => [
    { id: "started", accessorKey: "startedAt", header: "Started", enableHiding: false, cell: ({ row }) => <Timestamp value={row.original.startedAt} /> },
    { id: "stage", accessorKey: "stage", header: "Stage", filterFn: inSet },
    { id: "model", accessorKey: "model", header: "Model", filterFn: inSet, cell: ({ row }) => <span className="task-stage"><strong className="mono">{row.original.model || "—"}</strong><small>{apiLabels[row.original.api] ?? row.original.api}{row.original.streamed ? " · streamed" : ""}</small></span> },
    { id: "route", accessorKey: "routeKind", header: "Route", cell: ({ row }) => <span className="task-stage"><strong>{routeKindLabels[row.original.routeKind] ?? row.original.routeKind}</strong><small>{row.original.retryCount ? `${row.original.retryCount} retries` : "No retries or failovers"}</small></span> },
    { id: "status", accessorKey: "status", header: "Status", filterFn: inSet, cell: ({ row }) => <span className={`state-badge ${row.original.status === "ok" ? "state-succeeded" : row.original.httpStatus === 429 ? "state-waiting" : "state-failed"}`}>{row.original.status === "ok" ? "OK" : row.original.status} · {row.original.httpStatus}</span> },
    { id: "ttft", accessorKey: "ttftMs", header: "First token", cell: ({ row }) => <span className="num">{row.original.ttftMs >= 0 ? `${(row.original.ttftMs / 1000).toFixed(2)} s` : "—"}</span> },
    { id: "duration", accessorKey: "durationMs", header: "Duration", cell: ({ row }) => <span className="num">{(row.original.durationMs / 1000).toFixed(1)} s</span> },
    { id: "tokens", accessorFn: (c) => totalTokens(c.totals), header: "Tokens", cell: ({ row }) => row.original.usageReported
      ? <span className="num" title={`${row.original.totals?.inputTokens} in · ${row.original.totals?.outputTokens} out · ${row.original.totals?.cacheReadTokens} cache read · ${row.original.totals?.cacheWriteTokens} cache write`}>{compact(totalTokens(row.original.totals))}</span>
      : <span className="muted" title="The provider sent no usage block for this request">Not reported</span> },
    { id: "cost", accessorFn: (c) => Number(c.totals?.costUsdMicros ?? 0), header: "Est. cost", cell: ({ row }) => <span className="num">{usd(row.original.totals?.costUsdMicros ?? 0n)}</span> },
  ], []);
  if (cost.isPending) return <StatePanel kind="loading" title="Loading cost" />;
  if (cost.isError || !cost.data) return <StatePanel kind="error" title="Cost is unavailable" retry={() => void cost.refetch()} />;
  const c = cost.data;
  const t = c.totals!;
  const cacheable = Number(t.inputTokens) + Number(t.cacheReadTokens) + Number(t.cacheWriteTokens);
  const facet = (id: string, label: string, values: string[]) => ({ id, label, options: [...new Set(values)].sort().map((v) => ({ value: v, label: v })) });
  return <>
    <section className="stat-row" aria-label="Run cost summary">
      <StatTile label="Estimated cost" value={usd(t.costUsdMicros)} meta="List or contracted rates" />
      <StatTile label="Tokens" value={compact(totalTokens(t))} meta={`${compact(t.inputTokens)} in · ${compact(t.outputTokens)} out`} />
      <StatTile label="Cache hit" value={percent(Number(t.cacheReadTokens), cacheable)} meta={`${compact(t.cacheReadTokens)} cached input tokens`} />
      <StatTile label="Requests" value={compact(t.requests)} meta={`${compact(t.errors)} failed`} tone={Number(t.errors) ? "attention" : undefined} />
    </section>
    {!c.stages.length ? <Card title="No metered requests" description="This run has no model requests through the gateway yet. Stages that receive provider keys directly are not metered." /> : <>
      <Card title="Per stage" description="Estimated cost, tokens and prompt-cache hits for each stage.">
        <CollectionTable id="run-cost-stages" label="Cost per stage" columns={stageColumns} data={c.stages} getRowId={(s) => s.taskId + s.stage}
          view={stageView} onView={setStageView} empty="No stages." noun="stages" paged={false} defaultHidden={["input", "output"]} />
      </Card>
      <Card title="Requests" description={c.truncated ? "The newest 500 model requests. Route, retries and failovers are recorded per request." : "Every model request, newest first. Route, retries and failovers are recorded per request."}>
        <CollectionTable id="run-cost-requests" label="Model requests" columns={callColumns} data={c.requests} getRowId={(r) => `${r.startedAt}-${r.stage}-${r.durationMs}-${r.httpStatus}`}
          view={callView} onView={setCallView} empty="No requests." noun="requests" defaultHidden={["duration"]}
          facets={[facet("stage", "Stage", c.requests.map((r) => r.stage)), facet("model", "Model", c.requests.map((r) => r.model)), facet("status", "Status", c.requests.map((r) => r.status))]} />
      </Card>
    </>}
  </>;
}
