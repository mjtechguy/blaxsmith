// Page templates. DashboardLayout: stat tiles, then focused cards and tables.
// DetailLayout: a header with status, key facts, and actions over route-backed
// tabs. SettingsLayout: a section sub-nav with one section per view and a
// sticky save bar guarded against losing edits. CreateFlow: short steps with
// a summary aside.
import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, useBlocker } from "@tanstack/react-router";
import { ArrowLeft, Check, CircleDot, RefreshCw } from "lucide-react";
import { PageHeader, PageShell } from "./page";
import { RouteTabs, useModalDialog, type TabSpec } from "./ui";

export function DashboardLayout({ title, description, actions, tiles, slot, children }: {
  title: string; description?: string; actions?: ReactNode; tiles?: ReactNode; slot?: ReactNode; children: ReactNode;
}) {
  return <PageShell>
    <PageHeader title={title} description={description} actions={actions} />
    {tiles ? <section className="stat-row" aria-label="Summary">{tiles}</section> : null}
    {slot}
    <div className="dash-grid">{children}</div>
  </PageShell>;
}

export type Fact = { label: string; value: ReactNode };

export function DetailLayout({ back, title, status, facts = [], actions, slot, tabs, current, tabsLabel, children }: {
  back?: { href: string; label: string }; title: ReactNode; status?: ReactNode; facts?: Fact[]; actions?: ReactNode; slot?: ReactNode;
  tabs?: TabSpec[]; current?: string; tabsLabel?: string; children: ReactNode;
}) {
  return <PageShell>
    <header className="detail-header">
      {back ? <Link to={back.href as "/"} className="text-action detail-back"><ArrowLeft size={14} aria-hidden="true" /> {back.label}</Link> : null}
      <div className="detail-title-row">
        <div className="detail-title"><h1>{title}</h1>{status}</div>
        {actions ? <div className="page-actions">{actions}</div> : null}
      </div>
      {facts.length ? <dl className="detail-facts">{facts.map((fact) => <div key={fact.label}><dt>{fact.label}</dt><dd>{fact.value}</dd></div>)}</dl> : null}
      {slot}
    </header>
    {tabs && current ? <RouteTabs tabs={tabs} current={current} label={tabsLabel ?? "Sections"} /> : null}
    <div className="detail-body">{children}</div>
  </PageShell>;
}

export type SettingsSection = { id: string; label: string; href: string; soon?: boolean };

export function SettingsLayout({ title, description, sections, current, children }: {
  title: string; description?: string; sections: SettingsSection[]; current: string; children: ReactNode;
}) {
  return <PageShell>
    <PageHeader title={title} description={description} />
    <div className="settings-layout">
      <nav className="settings-nav" aria-label={`${title} sections`}>
        {sections.map((section) => <Link key={section.id} to={section.href as "/"} activeOptions={{ exact: true, includeSearch: false }} className="settings-link" aria-current={section.id === current ? "page" : undefined}>
          <span>{section.label}</span>{section.soon ? <span className="soon-badge">Soon</span> : null}</Link>)}
      </nav>
      <div className="settings-content">{children}</div>
    </div>
  </PageShell>;
}

// Blocks in-app navigation away from unsaved edits with the shared confirmation
// dialog. No native beforeunload prompt: a browser reload may discard edits.
export function useUnsavedGuard(dirty: boolean) {
  const bypass = useRef(false);
  const blocker = useBlocker({
    shouldBlockFn: ({ current, next }) => {
      if (bypass.current) { bypass.current = false; return false; }
      return dirty && current.pathname !== next.pathname;
    },
    enableBeforeUnload: false, withResolver: true,
  });
  return {
    allowNextNavigation: () => { bypass.current = true; },
    dialog: blocker.status === "blocked" ? <ConfirmDiscard onStay={() => blocker.reset()} onLeave={() => blocker.proceed()} /> : null,
  };
}

function ConfirmDiscard({ onStay, onLeave }: { onStay: () => void; onLeave: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useModalDialog(dialog);
  return <dialog ref={dialog} className="review-confirm" aria-labelledby="discard-title" onCancel={(event) => { event.preventDefault(); onStay(); }}>
    <h3 id="discard-title">Discard unsaved changes?</h3>
    <p>Your edits on this page have not been saved. Leaving now discards them.</p>
    <div className="review-confirm-actions">
      <button type="button" className="secondary-button" autoFocus onClick={onStay}>Keep editing</button>
      <button type="button" className="primary-button danger-button" onClick={onLeave}>Discard changes</button>
    </div>
  </dialog>;
}

// Sticky save/cancel bar for a settings section. It stays visible while the
// section scrolls and shows whether there is anything to save.
export function SaveBar({ dirty, saving, canSave = true, saveLabel = "Save changes", onCancel, error, saved }: {
  dirty: boolean; saving: boolean; canSave?: boolean; saveLabel?: string; onCancel: () => void; error?: string; saved?: boolean;
}) {
  return <div className="save-bar" role="region" aria-label="Save changes">
    <span className="save-state" role="status" aria-live="polite">{error ? <span className="form-field-error">{error}</span> : saving ? "Saving…" : dirty ? "Unsaved changes" : saved ? "Saved" : "No changes"}</span>
    <div className="save-actions">
      <button type="button" className="secondary-button" disabled={!dirty || saving} onClick={onCancel}>Cancel</button>
      <button type="submit" className="primary-button" disabled={!dirty || saving || !canSave}>{saving ? <RefreshCw size={15} className="spin" aria-hidden="true" /> : null}{saving ? "Saving…" : saveLabel}</button>
    </div>
  </div>;
}

// SaveBar plus the unsaved-change guard, for a settings section's form.
export function GuardedSaveBar(props: Parameters<typeof SaveBar>[0]) {
  const guard = useUnsavedGuard(props.dirty && !props.saving);
  return <><SaveBar {...props} />{guard.dialog}</>;
}

export type FlowStep = { id: string; label: string; state: "done" | "current" | "todo"; href?: string; search?: Record<string, string> };

export function CreateFlow({ title, description, steps, summary, back, children }: {
  title: string; description?: string; steps: FlowStep[]; summary?: ReactNode; back?: { href: string; label: string }; children: ReactNode;
}) {
  const index = steps.findIndex((s) => s.state === "current");
  return <PageShell>
    <PageHeader title={title} description={description} />
    {back ? <Link to={back.href as "/"} className="text-action"><ArrowLeft size={14} aria-hidden="true" /> {back.label}</Link> : null}
    <div className="flow-layout">
      <div className="flow-main">
        <ol className="flow-steps" aria-label={`Step ${index + 1} of ${steps.length}`}>
          {steps.map((step, i) => {
            const body = <><span className="flow-mark" aria-hidden="true">{step.state === "done" ? <Check size={13} /> : step.state === "current" ? <CircleDot size={13} /> : i + 1}</span><span>{step.label}</span></>;
            return <li key={step.id} className={`flow-step is-${step.state}`} aria-current={step.state === "current" ? "step" : undefined}>
              {step.href && step.state !== "current" ? <Link to={step.href as "/"} search={step.search as never}>{body}</Link> : <span>{body}</span>}
              <span className="sr-only">{step.state === "done" ? " (done)" : step.state === "todo" ? " (not started)" : ""}</span></li>;
          })}
        </ol>
        {children}
      </div>
      {summary ? <aside className="flow-summary" aria-label="Summary">{summary}</aside> : null}
    </div>
  </PageShell>;
}

// A key/value summary used by CreateFlow asides and overview cards.
export function SummaryList({ items }: { items: Array<{ label: string; value: ReactNode; done?: boolean }> }) {
  return <dl className="summary-list">{items.map((item) => <div key={item.label} className={item.done === undefined ? undefined : item.done ? "is-done" : "is-todo"}>
    <dt>{item.label}</dt><dd>{item.value}</dd></div>)}</dl>;
}

export function useSaved() {
  const [saved, setSaved] = useState(false);
  useEffect(() => { if (!saved) return; const t = window.setTimeout(() => setSaved(false), 4000); return () => window.clearTimeout(t); }, [saved]);
  return [saved, setSaved] as const;
}
