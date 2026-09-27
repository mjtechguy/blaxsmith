import { tableFeatures, useTable, type ColumnDef } from "@tanstack/react-table";
import { DataTable } from "./data-table";
import { useEffect, useMemo, useState } from "react";
import { useQuery, useInfiniteQuery, useMutation } from "@tanstack/react-query";
import type { RunEvidence } from "./gen/blaxsmith/api/v1/workflow_pb";
import { getEvidenceContent, listRunEvidence, getDeliveryReport } from "./workflow";
import { Markdown } from "./markdown";

function metadata(record: RunEvidence): Record<string, unknown> {
  try { return JSON.parse(record.metadataJson) as Record<string, unknown>; } catch { return {}; }
}

export function RunEvidencePanel({ runId, scope }: { runId: string; scope: string }) {
  const records = useInfiniteQuery({ queryKey: ["run-evidence", scope, runId], initialPageParam: "", queryFn: ({ pageParam, signal }) => listRunEvidence(runId, pageParam, signal), getNextPageParam: (page) => page.nextAfterId || undefined });
  const items = records.data?.pages.flatMap((page) => page.evidence) ?? [];
  return <section className="editor-card" aria-labelledby="run-evidence-heading">
    <div className="editor-card-heading"><div><h2 id="run-evidence-heading">Checks &amp; evidence</h2>
      <p>Platform checks run frozen commands on the candidate revision. Agent checks report the team’s findings; required checks must pass before approval.</p></div></div>
    <DeliveryExport runId={runId} />
    {records.isPending ? <p role="status">Loading evidence…</p> : null}
    {records.isError ? <p role="alert">Evidence could not be loaded. <button type="button" className="text-action" onClick={() => void records.refetch()}>Try again</button></p> : null}
    {records.isSuccess && items.length === 0 ? <p>No evidence has been collected. Only required checks in the frozen policy block acceptance.</p> : null}
    {items.map((record) => <EvidenceItem key={record.id} record={record} runId={runId} scope={scope} />)}
    {records.hasNextPage ? <button type="button" className="secondary-button" disabled={records.isFetchingNextPage} onClick={() => void records.fetchNextPage()}>Load more evidence</button> : null}
  </section>;
}

function EvidenceItem({ record, runId, scope }: { record: RunEvidence; runId: string; scope: string }) {
  const [open, setOpen] = useState(false);
  const meta = metadata(record);
  const label = typeof meta.title === "string" ? meta.title : typeof meta.check === "string" ? meta.check : record.key;
  const source = record.kind === "verification" ? "Platform check" : record.kind === "gate" ? "Agent check" : "Artifact";
  const verdict = meta.accepted === false ? "refused" : typeof meta.verdict === "string" ? meta.verdict : "collected";
  return <details className="observation-provenance" onToggle={(event) => setOpen(event.currentTarget.open)}>
    <summary>{source}: {label} · {verdict}{typeof meta.mode === "string" ? ` · ${meta.mode}` : ""}{record.current ? "" : " · previous attempt"}</summary>
    <p>Stage {record.stage} · revision <code>{record.revision}</code></p>
    {typeof meta.summary === "string" ? <p>{meta.summary}</p> : null}
    {typeof meta.reason === "string" && meta.reason ? <p>{meta.reason}</p> : null}
    {record.kind !== "gate" ? <p>SHA-256: <code>{record.sha256}</code></p> : null}
    {open && record.kind !== "gate" ? <EvidenceContent record={record} runId={runId} scope={scope} renderer={String(meta.renderer ?? "text")} title={label} /> : null}
  </details>;
}

function EvidenceContent({ record, runId, scope, renderer, title }: { record: RunEvidence; runId: string; scope: string; renderer: string; title: string }) {
  const result = useQuery({ queryKey: ["evidence-content", scope, runId, record.id], queryFn: ({ signal }) => getEvidenceContent(runId, record.id, signal), staleTime: Infinity });
  const [url, setUrl] = useState("");
  useEffect(() => {
    if (!result.data) return;
    const blob = new Blob([new Uint8Array(result.data.content)], { type: result.data.contentType });
    const next = URL.createObjectURL(blob); setUrl(next);
    return () => URL.revokeObjectURL(next);
  }, [result.data]);
  if (result.isPending) return <p role="status">Loading content…</p>;
  if (result.isError) return <p role="alert">Content is unavailable. <button type="button" className="text-action" onClick={() => void result.refetch()}>Try again</button></p>;
  const data = result.data!;
  const text = new TextDecoder().decode(data.content);
  // ponytail: large reports remain downloadable; cap interactive rendering to
  // 128 KiB until report pagination is needed.
  const bounded = data.content.length <= 128 * 1024;
  return <div>
    {url ? <a className="text-action" href={url} download={`${record.key}.${renderer === "image" ? data.contentType.split("/")[1] : renderer === "json" || renderer === "table" ? "json" : "txt"}`}>Download {title}</a> : null}
    {renderer === "image" && url ? <img src={url} alt={title} className="evidence-image" />
      : !bounded ? <p>This report is too large to preview. Download it to read the complete content.</p>
        : renderer === "markdown" ? <Markdown text={text} />
          : renderer === "table" ? <JSONTable text={text} />
            : <pre className="code-block">{renderer === "json" ? JSON.stringify(JSON.parse(text), null, 2) : text || "No command output."}</pre>}
  </div>;
}

const evidenceFeatures = tableFeatures({});
function JSONTable({ text }: { text: string }) {
  const value: unknown = useMemo(() => JSON.parse(text), [text]);
  const rows = useMemo(() => Array.isArray(value) && value.length <= 200 && value.every((row) => row && typeof row === "object" && !Array.isArray(row)) ? value as Record<string, unknown>[] : [], [value]);
  const columns = useMemo<ColumnDef<typeof evidenceFeatures, Record<string, unknown>>[]>(() => [...new Set(rows.flatMap(Object.keys))].slice(0, 20).map((key) => ({ id: key, header: key, accessorFn: (row) => typeof row[key] === "string" ? row[key] : JSON.stringify(row[key] ?? null) })), [rows]);
  const table = useTable({ features: evidenceFeatures, data: rows, columns });
  if (rows.length === 0 || new Set(rows.flatMap(Object.keys)).size > 20) return <pre className="code-block">{JSON.stringify(value, null, 2)}</pre>;
  return <DataTable table={table} label="Evidence ledger" />;
}

function DeliveryExport({ runId }: { runId: string }) {
 const report = useMutation({ mutationFn: () => getDeliveryReport(runId) });
 const [url, setURL] = useState("");
 useEffect(() => {
  if (!report.data) return;
  const value = URL.createObjectURL(new Blob([report.data.markdown], { type: "text/markdown;charset=utf-8" }));
  setURL(value); return () => URL.revokeObjectURL(value);
 }, [report.data]);
 return <div className="delivery-export"><button type="button" className="secondary-button" disabled={report.isPending} onClick={() => report.mutate()}>{report.isPending ? "Preparing export…" : "Prepare delivery export"}</button>
  <p className="form-hint">Includes goal text, plans, frozen checks and evidence references. Review project content before sharing. Raw logs, artifact bodies and platform credentials are excluded.</p>
  {report.isError ? <p role="alert">Export could not be prepared. Retry, or use the paginated evidence view for a large run.</p> : null}
  {report.data && url ? <><a className="text-action" href={url} download={`blaxsmith-${runId}-${report.data.sha256.slice(0,12)}.md`}>Download delivery snapshot (.md)</a><p>Snapshot {new Date(report.data.generatedAt).toLocaleString()} · SHA-256 <code>{report.data.sha256}</code></p><details className="observation-provenance"><summary>Preview export</summary><label className="form-field"><span>Delivery Markdown</span><textarea rows={12} readOnly value={report.data.markdown} /></label></details></> : null}
 </div>;
}
