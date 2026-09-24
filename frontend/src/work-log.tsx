import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import { Activity, Bot, Check, ChevronRight, Circle, CircleDashed, FilePen, ListChecks, Repeat, SquareTerminal, Wrench, X } from "lucide-react";
import {
  attentionTitle, formatDuration, groupSummary, planProgress, statusLabels, workLog,
  type AgentStatus, type PlanItem, type Progress, type ToolRow, type WorkEntry,
} from "./agent-view";

export function StatusPill({ status }: { status: AgentStatus | null }) {
  if (!status) return <span className="status-pill status-none">—</span>;
  return <span className={`status-pill status-${status}`}>{statusLabels[status]}</span>;
}

// Prefixes the tab title with the number of things waiting on the viewer and
// sets the app badge where the browser supports it (installed PWAs).
// null leaves the title alone (a parent route while a child route owns it).
export function useAttentionTitle(count: number | null) {
  useEffect(() => {
    if (count === null) return;
    const base = document.title.replace(/^\(\d+\+?\) /, "");
    document.title = attentionTitle(count, base);
    const nav = navigator as Navigator & { setAppBadge?: (n?: number) => Promise<void>; clearAppBadge?: () => Promise<void> };
    if (count > 0) void nav.setAppBadge?.(count).catch(() => undefined);
    else void nav.clearAppBadge?.().catch(() => undefined);
    return () => { document.title = base; void nav.clearAppBadge?.().catch(() => undefined); };
  }, [count]);
}

const statusIcon = (status: string) => status === "failed" ? <X size={13} aria-hidden="true" />
  : status === "running" ? <CircleDashed size={13} aria-hidden="true" /> : <Check size={13} aria-hidden="true" />;
const statusText = (status: string) => status === "failed" ? "Failed" : status === "running" ? "Running" : "Completed";

function ToolGroup({ items }: { items: ToolRow[] }) {
  const [open, setOpen] = useState(false);
  const id = useId();
  const failed = items.some((item) => item.status === "failed");
  const running = items.some((item) => item.status === "running");
  if (items.length === 1) return <ToolLine item={items[0]} />;
  return <li className="work-row work-group">
    <button type="button" className="work-group-toggle" aria-expanded={open} aria-controls={id} onClick={() => setOpen(!open)}>
      <ChevronRight size={14} className={open ? "work-chevron is-open" : "work-chevron"} aria-hidden="true" />
      <span className={`work-mark ${failed ? "is-failed" : running ? "is-running" : ""}`}><Wrench size={13} aria-hidden="true" /></span>
      <span className="work-main"><strong>{groupSummary(items)}</strong>
        <small>{[...new Set(items.map((item) => item.tool))].slice(0, 4).join(", ")}</small></span>
    </button>
    <ul id={id} className="work-sublist" hidden={!open}>{items.map((item) => <ToolLine key={item.key} item={item} />)}</ul>
  </li>;
}

function ToolLine({ item }: { item: ToolRow }) {
  return <li className="work-row">
    <span className={`work-mark is-${item.status}`} title={statusText(item.status)}>{statusIcon(item.status)}<span className="sr-only">{statusText(item.status)}</span></span>
    <span className="work-main"><strong>{item.tool}</strong>{item.input ? <code className="work-detail">{item.input}{item.truncated ? " (truncated)" : ""}</code> : null}</span>
    <span className="work-meta">{formatDuration(item.durationMs)}</span>
  </li>;
}

function Entry({ entry }: { entry: WorkEntry }) {
  switch (entry.kind) {
    case "tools": return <ToolGroup items={entry.items} />;
    case "command": {
      const exitText = entry.exit !== undefined ? `exit ${entry.exit}` : entry.status === "failed" ? "failed" : entry.status === "running" ? "running" : "";
      return <li className="work-row">
        <span className={`work-mark is-${entry.status}`}><SquareTerminal size={13} aria-hidden="true" /></span>
        <span className="work-main"><code className="work-cmd">$ {entry.cmd}{entry.truncated ? " …" : ""}</code></span>
        <span className="work-meta"><span className={`exit-badge ${entry.status === "failed" ? "is-failed" : entry.status === "running" ? "is-running" : "is-ok"}`}>{exitText}</span> {formatDuration(entry.durationMs)}</span>
      </li>;
    }
    case "file":
      return <li className="work-row">
        <span className={`work-mark is-${entry.status}`}><FilePen size={13} aria-hidden="true" /></span>
        <span className="work-main"><code className="work-path">{entry.path}</code>{entry.change ? <small>{entry.change}</small> : null}</span>
        <span className="work-meta">{entry.added !== undefined ? <><span className="diff-add">+{entry.added}</span> <span className="diff-del">−{entry.removed ?? 0}</span></> : entry.status === "failed" ? "failed" : null}</span>
      </li>;
    case "message":
      return <li className="work-row work-message">
        <span className="work-mark"><Bot size={13} aria-hidden="true" /></span>
        <p className="work-main">{entry.text}{entry.truncated ? " …" : ""}</p>
      </li>;
    case "note": {
      const p = entry.event.p;
      const title = entry.event.type === "cycle" ? `Cycle ${String(p.cycle ?? "")} · ${String(p.status ?? "")}` : entry.event.type;
      return <li className="work-row">
        <span className="work-mark">{entry.event.type === "cycle" ? <Repeat size={13} aria-hidden="true" /> : <Activity size={13} aria-hidden="true" />}</span>
        <span className="work-main"><strong className="work-note">{title}</strong><small>{String(p.text ?? p.message ?? p.name ?? "")}</small></span>
        <time className="work-meta" dateTime={entry.event.at}>{new Date(entry.event.at).toLocaleTimeString()}</time>
      </li>;
    }
  }
}

export function TasksBadge({ plan }: { plan: PlanItem[] }) {
  const { done, total } = planProgress(plan);
  return <span className="tasks-badge" aria-label={`${done} of ${total} tasks done`}><ListChecks size={13} aria-hidden="true" /> {done}/{total}</span>;
}

function PlanChecklist({ plan }: { plan: PlanItem[] }) {
  const { current } = planProgress(plan);
  return <section className="plan-card" aria-labelledby="plan-heading">
    <header><h3 id="plan-heading">Plan</h3><TasksBadge plan={plan} /></header>
    {current ? <p className="plan-current" aria-live="polite">Now: {current}</p> : null}
    <ol className="plan-list">{plan.map((item, index) => <li key={index} className={`plan-item is-${item.status}`}>
      {item.status === "completed" ? <Check size={13} aria-hidden="true" /> : item.status === "in_progress" ? <CircleDashed size={13} aria-hidden="true" /> : <Circle size={13} aria-hidden="true" />}
      <span>{item.text}</span><span className="sr-only"> ({item.status.replace("_", " ")})</span>
    </li>)}</ol>
  </section>;
}

// WorkLog is the default stage view: the agent's tools, commands, file
// changes, messages and plan from attempt.progress records.
// Oldest first; the list stays pinned to the newest entry unless scrolled up.
export function WorkLog({ events }: { events: Progress[] }) {
  const { entries, plan } = workLog(events);
  const list = useRef<HTMLOListElement>(null);
  const pinned = useRef(true);
  useLayoutEffect(() => { if (pinned.current && list.current) list.current.scrollTop = list.current.scrollHeight; }, [entries.length, events.length]);
  return <div className="work-log">
    {plan?.length ? <PlanChecklist plan={plan} /> : null}
    {entries.length === 0 ? <div className="table-empty">No agent activity yet. Tool calls, commands, and file changes appear here as the agent works.</div>
      : <ol ref={list} className="work-list" aria-label="Agent work log" tabIndex={0}
        onScroll={(event) => { const el = event.currentTarget; pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24; }}>
        {entries.map((entry) => <Entry key={entry.key} entry={entry} />)}</ol>}
  </div>;
}
