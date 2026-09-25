// Shared display primitives for progressive disclosure: stat tiles, cards,
// inline disclosures, copyable values, timestamps, "show more", empty states,
// and route-backed tabs. No drawers: detail opens inline or on a routed page.
import { useEffect, useId, useRef, useState, type ReactNode, type RefObject } from "react";
import { Link, useLocation } from "@tanstack/react-router";
import { Check, ChevronRight, Copy, RefreshCw } from "lucide-react";
import { ago } from "./admin";
import { usePrefs } from "./preferences";

// Status and badge text is sentence case: "reconnect_required" -> "Reconnect required".
export const sentence = (value: string) => { const text = value.replaceAll("_", " "); return text.charAt(0).toUpperCase() + text.slice(1); };

// Opens a <dialog> modally on mount. A dialog removed by React (rather than
// closed) does not return focus, so hand it back to whatever opened it unless
// something else, such as a navigation, has taken focus since.
export function useModalDialog(dialog: RefObject<HTMLDialogElement | null>) {
  // Captured on first render, before showModal moves focus into the dialog.
  const opener = useRef(globalThis.document?.activeElement);
  useEffect(() => {
    if (!dialog.current?.open) dialog.current?.showModal();
    return () => {
      const lost = !document.activeElement || document.activeElement === document.body;
      const target = opener.current;
      if (lost && target instanceof HTMLElement && target.isConnected) target.focus();
    };
  }, [dialog]);
}

export function StatTile({ label, value, meta, tone, href }: { label: string; value: ReactNode; meta?: ReactNode; tone?: "attention" | "danger" | "ok"; href?: string }) {
  const body = <><span className="stat-label">{label}</span><strong className="stat-value">{value}</strong>{meta ? <span className="stat-meta">{meta}</span> : null}</>;
  const className = `stat-tile${tone ? ` stat-${tone}` : ""}`;
  return href ? <Link to={href as "/"} className={`${className} stat-link`}>{body}</Link> : <div className={className}>{body}</div>;
}

export function Card({ title, description, actions, children, id, className = "" }: {
  title: ReactNode; description?: ReactNode; actions?: ReactNode; children?: ReactNode; id?: string; className?: string;
}) {
  const generated = useId();
  const headingId = id ?? generated;
  return <section className={`card ${className}`.trim()} aria-labelledby={headingId}>
    <header className="card-header"><div><h2 id={headingId}>{title}</h2>{description ? <p>{description}</p> : null}</div>{actions ? <div className="card-actions">{actions}</div> : null}</header>
    {children}
  </section>;
}

// Inline "Advanced" section: collapsed by default, a real button with aria-expanded.
export function Disclosure({ summary, children, defaultOpen = false, className = "" }: { summary: ReactNode; children: ReactNode; defaultOpen?: boolean; className?: string }) {
  const [isOpen, setOpen] = useState(defaultOpen);
  const panel = useId();
  return <div className={`disclosure ${className}`.trim()}>
    <button type="button" className="disclosure-toggle" aria-expanded={isOpen} aria-controls={panel} onClick={() => setOpen(!isOpen)}>
      <ChevronRight size={14} aria-hidden="true" className={isOpen ? "is-open" : undefined} />{summary}</button>
    {isOpen ? <div id={panel} className="disclosure-panel">{children}</div> : null}
  </div>;
}

export function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => { if (!copied) return; const t = window.setTimeout(() => setCopied(false), 1500); return () => window.clearTimeout(t); }, [copied]);
  return <button type="button" className="copy-button" aria-label={copied ? `${label} copied` : `Copy ${label}`}
    onClick={() => { void navigator.clipboard?.writeText(value).then(() => setCopied(true)).catch(() => undefined); }}>
    {copied ? <Check size={13} aria-hidden="true" /> : <Copy size={13} aria-hidden="true" />}<span>{copied ? "Copied" : "Copy"}</span></button>;
}

// A small anchored popover for identifiers and details. Opens on click or keyboard; closes on Escape or outside click.
export function InfoPopover({ trigger, label, children, className = "" }: { trigger: ReactNode; label: string; children: ReactNode; className?: string }) {
  const [isOpen, setOpen] = useState(false);
  const panel = useId();
  const root = useRef<HTMLSpanElement>(null);
  const button = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    if (!isOpen) return;
    const outside = (event: MouseEvent) => { if (!root.current?.contains(event.target as Node)) setOpen(false); };
    const key = (event: KeyboardEvent) => { if (event.key === "Escape") { setOpen(false); button.current?.focus(); } };
    document.addEventListener("mousedown", outside);
    document.addEventListener("keydown", key);
    return () => { document.removeEventListener("mousedown", outside); document.removeEventListener("keydown", key); };
  }, [isOpen]);
  return <span className={`info ${className}`.trim()} ref={root}>
    <button ref={button} type="button" className="info-trigger" aria-expanded={isOpen} aria-controls={panel} aria-label={label} onClick={() => setOpen(!isOpen)}>{trigger}</button>
    {isOpen ? <span id={panel} className="info-panel" role="group" aria-label={label}>{children}</span> : null}
  </span>;
}

// An ID or digest shown short, with the full value and a copy button one click away.
export function CopyValue({ value, label, chars = 8 }: { value: string; label: string; chars?: number }) {
  if (!value) return <span className="muted">—</span>;
  const short = value.length > chars + 1 ? `${value.slice(0, chars)}…` : value;
  return <InfoPopover label={`${label}: show full value`} trigger={<code>{short}</code>}>
    <span className="info-title">{label}</span><code className="info-value">{value}</code><CopyButton value={value} label={label} />
  </InfoPopover>;
}

const useShownTime = (value: string, now?: number) => {
  const { dateStyle } = usePrefs();
  const date = new Date(value);
  return dateStyle === "absolute" ? date.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }) : ago(value, now);
};

// The same preference-aware time as Timestamp, as plain text for places that
// cannot hold a button (inside a link row); the full time is the tooltip.
export function TimeText({ value, now }: { value: string; now?: number }) {
  const shown = useShownTime(value, now);
  if (!value) return <span className="muted">—</span>;
  return <time dateTime={value} title={new Date(value).toLocaleString()}>{shown}</time>;
}

export function Timestamp({ value, now }: { value: string; now?: number }) {
  const shown = useShownTime(value, now);
  if (!value) return <span className="muted">—</span>;
  const date = new Date(value);
  return <InfoPopover label={`Time: ${date.toLocaleString()}`} trigger={<time dateTime={value}>{shown}</time>}>
    <span className="info-title">Local</span><span className="info-value">{date.toLocaleString()}</span>
    <span className="info-title">UTC</span><code className="info-value">{date.toISOString()}</code><CopyButton value={date.toISOString()} label="timestamp" />
  </InfoPopover>;
}

export function ShowMore<T>({ items, initial = 5, render, noun = "more" }: { items: T[]; initial?: number; render: (item: T, index: number) => ReactNode; noun?: string }) {
  const [all, setAll] = useState(false);
  const shown = all ? items : items.slice(0, initial);
  return <>{shown.map(render)}{items.length > initial ? <button type="button" className="text-action show-more" aria-expanded={all} onClick={() => setAll(!all)}>
    {all ? "Show fewer" : `Show ${items.length - initial} ${noun}`}</button> : null}</>;
}

export function EmptyState({ icon, title, children, action }: { icon?: ReactNode; title: string; children?: ReactNode; action?: ReactNode }) {
  return <div className="empty-state">{icon ? <span className="empty-icon">{icon}</span> : null}<h3>{title}</h3>{children ? <p>{children}</p> : null}{action ? <div className="empty-action">{action}</div> : null}</div>;
}

export function StatePanel({ kind, title, children, retry }: { kind: "loading" | "error" | "note"; title: string; children?: ReactNode; retry?: () => void }) {
  return <div className="state-panel" role={kind === "error" ? "alert" : kind === "loading" ? "status" : "note"}>
    {kind === "loading" ? <RefreshCw className="spin" size={22} aria-hidden="true" /> : null}<h2>{title}</h2>{children ? <p>{children}</p> : null}
    {retry ? <button type="button" className="secondary-button" onClick={retry}>Try again</button> : null}</div>;
}

export type TabSpec = { id: string; label: string; count?: number; hidden?: boolean };

// Route-backed tabs: each tab is a link that sets ?tab=, so tabs are shareable and survive reload.
export function RouteTabs({ tabs, current, label }: { tabs: TabSpec[]; current: string; label: string }) {
  const pathname = useLocation({ select: (l) => l.pathname });
  return <nav className="route-tabs" aria-label={label}>
    {tabs.filter((t) => !t.hidden).map((tab, index) => <Link key={tab.id} to={pathname as "/"} replace activeOptions={{ exact: true, includeSearch: true }}
      search={((prev: Record<string, unknown>) => { const { stage: _stage, ...rest } = prev; const base = tab.id === "stages" ? prev : rest; return { ...base, tab: index === 0 ? undefined : tab.id }; }) as never}
      className="route-tab" aria-current={tab.id === current ? "page" : undefined}>{tab.label}{tab.count !== undefined ? <span className="tab-count">{tab.count}</span> : null}</Link>)}
  </nav>;
}

export function tabFrom(search: Record<string, unknown>, tabs: TabSpec[]): string {
  const wanted = typeof search.tab === "string" ? search.tab : "";
  return tabs.find((t) => t.id === wanted && !t.hidden)?.id ?? tabs[0].id;
}
