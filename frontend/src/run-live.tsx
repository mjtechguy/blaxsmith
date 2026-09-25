import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode, type Ref } from "react";
import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { Check, FileText, Hand, MessageSquare, TerminalSquare, X } from "lucide-react";
import type { RunTask, WorkflowEvent } from "./gen/blaxsmith/api/v1/workflow_pb";
import {
  answerInteraction, eventPayload, getAttemptControl, handBackAttempt, listInteractions, steerAttempt, takeOverAttempt,
  type Interaction, type SteerKind, type TerminalState,
} from "./run-control";
import { Markdown } from "./markdown";
import { AttemptTerminal, type TerminalLink } from "./terminal";
import { approvalChoices, cardKey, workLog, type AgentStatus, type Progress } from "./agent-view";
import { StatusPill, TasksBadge, WorkLog } from "./work-log";

type StageTab = "work" | "terminal";

function errorText(cause: unknown, fallback: string) {
  const code = ConnectError.from(cause).code;
  return code === Code.FailedPrecondition ? "This changed since you loaded it. Check the latest state and try again."
    : code === Code.PermissionDenied ? "Your session is not allowed to do this." : fallback;
}

// In-app confirmation modal (no browser dialogs). Content renders only while open.
export function ConfirmModal({ open, title, children, confirmLabel, pending, disabled, error, onConfirm, onCancel }: {
  open: boolean; title: string; children?: ReactNode; confirmLabel: string; pending?: boolean; disabled?: boolean; error?: string;
  onConfirm: () => void; onCancel: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const titleId = useId();
  useEffect(() => { if (open) dialog.current?.showModal(); else dialog.current?.close(); }, [open]);
  return <dialog ref={dialog} className="review-confirm" aria-labelledby={titleId} onCancel={(event) => { event.preventDefault(); if (!pending) onCancel(); }}>
    {open ? <>
      <h3 id={titleId}>{title}</h3>
      {children}
      {error ? <p className="auth-alert" role="alert">{error}</p> : null}
      <div className="review-confirm-actions">
        <button type="button" className="secondary-button" disabled={pending} onClick={onCancel}>Cancel</button>
        <button type="button" className="primary-button" disabled={pending || disabled} onClick={onConfirm}>{pending ? "Saving…" : confirmLabel}</button>
      </div>
    </> : null}
  </dialog>;
}

function TextArea({ label, value, onChange, placeholder, hint, inputRef, describedBy }: {
  label: string; value: string; onChange: (value: string) => void; placeholder: string; hint?: string;
  inputRef?: Ref<HTMLTextAreaElement>; describedBy?: string;
}) {
  const id = useId();
  return <div className="review-feedback-input"><label htmlFor={id}>{label}</label>
    <textarea ref={inputRef} id={id} value={value} rows={4} maxLength={4000} placeholder={placeholder} aria-describedby={describedBy} onChange={(event) => onChange(event.target.value)} />
    {hint ? <small>{hint}</small> : null}</div>;
}

export const interactionsKey = (scope: string, runId: string) => ["run-interactions", scope, runId];
// Refetched on `interaction.*` events from the run's existing SSE stream.
export const useInteractions = (runId: string, scope: string, enabled: boolean) =>
  useQuery({ queryKey: interactionsKey(scope, runId), queryFn: () => listInteractions(runId), enabled });

const kindLabels: Record<string, string> = { question: "Question", approval: "Approval gate", escalation: "Escalation", interview_round: "Interview" };
const kindLabel = (kind: string) => kindLabels[kind] ?? kind.replaceAll("_", " ");

export function QuestionCard({ item, mayAnswer, scope, runId }: { item: Interaction; mayAnswer: boolean; scope: string; runId: string }) {
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<string[]>(() => item.options.filter((option) => option.recommended && !item.multiSelect).map((option) => option.id).slice(0, 1));
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const name = useId();
  const hintId = useId();
  const optionRefs = useRef<Array<HTMLInputElement | null>>([]);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const answer = useMutation({
    mutationFn: (reply: { optionIds: string[]; text: string }) => answerInteraction(item.id, reply.optionIds, reply.text),
    onSuccess: () => { setError(""); void queryClient.invalidateQueries({ queryKey: interactionsKey(scope, runId) }); },
    onError: (cause) => { setError(errorText(cause, "The answer could not be sent. Please try again.")); void queryClient.invalidateQueries({ queryKey: interactionsKey(scope, runId) }); },
  });
  const open = item.state === "open";
  const toggle = (id: string) => setSelected((old) => item.multiSelect ? (old.includes(id) ? old.filter((other) => other !== id) : [...old, id]) : [id]);
  const canSend = open && mayAnswer && !answer.isPending && (selected.length > 0 || (item.allowFreeText && text.trim().length > 0));
  const send = () => { if (canSend) answer.mutate({ optionIds: selected, text }); };
  const { approve, decline } = item.kind === "approval" ? approvalChoices(item.options) : {};
  const isApproval = item.kind === "approval" && (approve !== undefined || (item.options.length === 0 && item.allowFreeText));
  const decide = (option: typeof approve, fallback: string) => {
    if (!open || !mayAnswer || answer.isPending) return;
    answer.mutate(option ? { optionIds: [option.id], text } : { optionIds: [], text: text.trim() || fallback });
  };
  const keyed = Math.min(item.options.length, 9);
  const onKeyDown = (event: KeyboardEvent<HTMLElement>) => {
    if (!open || !mayAnswer || answer.isPending) return;
    const target = event.target as HTMLElement;
    if (target.closest("button, a, summary")) return; // their own Enter/Space
    const key = cardKey(event.key, keyed, target === textRef.current,
      { shift: event.shiftKey, ctrl: event.ctrlKey, meta: event.metaKey, alt: event.altKey, composing: event.nativeEvent.isComposing });
    if (!key) return;
    event.preventDefault();
    if (key.action === "select") {
      toggle(item.options[key.index].id);
      optionRefs.current[key.index]?.focus();
    } else if (key.action === "submit") {
      send();
    } else {
      setText("");
      const index = Math.max(0, item.options.findIndex((option) => selected.includes(option.id)));
      if (optionRefs.current[index]) optionRefs.current[index]!.focus(); else textRef.current?.blur();
    }
  };
  const approveOrDecline = selected.length === 1 && (selected[0] === approve?.id || selected[0] === decline?.id);

  return <article className={`ix-card ${item.blocking && open ? "ix-blocking" : ""}`} aria-label={item.title} onKeyDown={onKeyDown}>
    <header className="ix-heading">
      <span className="state-badge">{kindLabel(item.kind)}{item.interview ? ` · round ${item.interview.round}` : ""}</span>
      <span className="ix-stage">Stage {item.stage}</span>
      {item.blocking && open ? <span className={`status-pill ${item.kind === "approval" ? "status-needs_approval" : "status-awaiting_input"}`}>{item.kind === "approval" ? "Needs approval" : "Agent waiting"}</span> : null}
    </header>
    <h3>{item.title}</h3>
    {item.bodyMd ? <Markdown className="ix-body md" text={item.bodyMd} /> : null}
    {item.sources.length ? <ul className="ix-sources" aria-label="Sources">{item.sources.map((source) =>
      <li key={`${source.path}:${source.line ?? ""}`}><FileText size={13} aria-hidden="true" /><code>{source.path}{source.line ? `:${source.line}` : ""}</code></li>)}</ul> : null}
    {open ? <fieldset className="ix-options" disabled={!mayAnswer || answer.isPending} aria-describedby={mayAnswer ? hintId : undefined}>
      <legend className="sr-only">{item.multiSelect ? "Choose any" : "Choose one"}</legend>
      {item.options.map((option, index) => <label key={option.id} className={`ix-option ${selected.includes(option.id) ? "is-selected" : ""}`}>
        <input ref={(el) => { optionRefs.current[index] = el; }} type={item.multiSelect ? "checkbox" : "radio"} name={name}
          checked={selected.includes(option.id)} onChange={() => toggle(option.id)} aria-keyshortcuts={index < 9 ? String(index + 1) : undefined} />
        <span><strong>{option.label}{option.recommended ? <span className="version-badge">Recommended</span> : null}</strong>
          {option.description ? <small>{option.description}</small> : null}</span>
        {index < 9 && mayAnswer ? <kbd className="ix-key" aria-hidden="true">{index + 1}</kbd> : null}
      </label>)}
      {item.allowFreeText ? <TextArea inputRef={textRef} describedBy={mayAnswer ? hintId : undefined} label={item.options.length ? "Or add your own answer" : "Your answer"}
        value={text} onChange={setText} placeholder="Type a reply for the agent." /> : null}
    </fieldset> : <p className="ix-answer">{item.state === "answered" ? <>Answered{item.answer ? `: ${item.answer.optionIds.map((id) => item.options.find((option) => option.id === id)?.label ?? id).join(", ")}${item.answer.text ? ` — “${item.answer.text}”` : ""}` : ""}</> : `This ${kindLabel(item.kind).toLowerCase()} was ${item.state}.`}</p>}
    {error ? <p className="auth-alert" role="alert">{error}</p> : null}
    {open && mayAnswer ? <div className="ix-actions">
      <p id={hintId} className="ix-keys">
        {keyed ? <><kbd>1</kbd>{keyed > 1 ? <>–<kbd>{keyed}</kbd></> : null} choose · </> : null}<kbd>Enter</kbd> send
        {item.allowFreeText ? <> · <kbd>Shift</kbd>+<kbd>Enter</kbd> new line · <kbd>Esc</kbd> clear reply</> : null}
      </p>
      {isApproval ? <>
        {decline || item.options.length === 0 ? <button type="button" className="secondary-button" disabled={answer.isPending} onClick={() => decide(decline, "Declined")}><X size={15} aria-hidden="true" /> Decline</button> : null}
        {!approveOrDecline && canSend && (selected.length > 0 || text.trim()) && item.options.length > 0 ? <button type="button" className="secondary-button" onClick={send}>Send answer</button> : null}
        <button type="button" className="primary-button" disabled={answer.isPending} onClick={() => decide(approve, "Approved")}><Check size={15} aria-hidden="true" /> {answer.isPending ? "Sending…" : "Approve"}</button>
      </> : <button type="button" className="primary-button" disabled={!canSend} onClick={send}>{answer.isPending ? "Sending…" : "Send answer"}</button>}
    </div> : null}
    {open && !mayAnswer ? <p className="review-message">Your role can view this question but not answer it.</p> : null}
  </article>;
}

export function Inbox({ items, mayAnswer, scope, runId, projectId }: { items: Interaction[]; mayAnswer: boolean; scope: string; runId: string; projectId: string }) {
  const open = items.filter((item) => item.state === "open")
    .sort((a, b) => Number(b.blocking) - Number(a.blocking) || a.createdAt.localeCompare(b.createdAt));
  return <section className="table-section" aria-labelledby="inbox-heading">
    <div className="table-heading"><div><h2 id="inbox-heading">Inbox</h2><p>Questions, approval gates, and escalations raised by agents in this run. Blocking items come first.</p></div><span className="fetched-time">{open.length} open</span></div>
    {open.length === 0 ? <div className="table-empty">No agent is waiting on you.</div> : <div className="ix-list">{open.map((item) => item.kind === "interview_round"
      ? <div key={item.id} className="ix-card ix-pointer"><MessageSquare size={16} aria-hidden="true" /><span><strong>Interview round {item.interview?.round ?? ""} is waiting</strong><small>Stage {item.stage} · {item.title}</small></span>
        <Link from="/projects/$projectId/runs/$runId" to="/projects/$projectId/runs/$runId" params={{ projectId, runId }} search={{ stage: item.stage }} className="secondary-button">Open interview</Link></div>
      : <QuestionCard key={item.id} item={item} mayAnswer={mayAnswer} scope={scope} runId={runId} />)}</div>}
  </section>;
}

function InterviewView({ items, mayAnswer, scope, runId }: { items: Interaction[]; mayAnswer: boolean; scope: string; runId: string }) {
  const queryClient = useQueryClient();
  const rounds = items.slice().sort((a, b) => (a.interview?.round ?? 0) - (b.interview?.round ?? 0));
  const current = rounds.find((item) => item.state === "open");
  const [confirm, setConfirm] = useState(false);
  const finalize = useMutation({
    mutationFn: () => answerInteraction(current!.id, [current!.interview!.finalizeOption], ""),
    onSuccess: () => { setConfirm(false); void queryClient.invalidateQueries({ queryKey: interactionsKey(scope, runId) }); },
  });
  return <section className="table-section" aria-labelledby="interview-heading">
    <div className="table-heading"><div><h2 id="interview-heading">Interview</h2><p>One question per round. Finalize when the spec has what it needs.</p></div>
      {current?.interview?.finalizeOption && mayAnswer ? <button type="button" className="secondary-button" onClick={() => { finalize.reset(); setConfirm(true); }}>Finalize interview</button> : null}</div>
    <ol className="chat">{rounds.filter((item) => item !== current).map((item) => <li key={item.id}>
      <div className="chat-agent"><small>Round {item.interview?.round} · agent</small><strong>{item.title}</strong>{item.bodyMd ? <Markdown text={item.bodyMd} /> : null}</div>
      {item.answer ? <div className="chat-human"><small>You</small><p>{[item.answer.optionIds.map((id) => item.options.find((option) => option.id === id)?.label ?? id).join(", "), item.answer.text].filter(Boolean).join(" — ")}</p></div> : null}
    </li>)}</ol>
    {current ? <div className="chat-pinned"><QuestionCard key={current.id} item={current} mayAnswer={mayAnswer} scope={scope} runId={runId} /></div>
      : <div className="table-empty">No question is waiting. The agent is working on the next round.</div>}
    <ConfirmModal open={confirm} title="Finalize interview" confirmLabel="Finalize" pending={finalize.isPending}
      error={finalize.isError ? errorText(finalize.error, "The interview could not be finalized.") : undefined}
      onConfirm={() => finalize.mutate()} onCancel={() => setConfirm(false)}>
      <p>The agent stops asking questions and writes the spec from the {rounds.length - 1} answered rounds.</p>
    </ConfirmModal>
  </section>;
}

const num = (value: unknown) => typeof value === "number" ? value : Number(value) || 0;
const progressText = (event: Progress) => String(event.p.text ?? event.p.message ?? event.p.name ?? "");

function LoopPanel({ attemptId, events, task }: { attemptId: string; events: Progress[]; task: RunTask }) {
  const cycles = new Map<number, { status: string; findings: string[] }>();
  let cap = task.maxCycles;
  for (const event of events) {
    const cycle = num(event.p.cycle);
    const entry = cycles.get(cycle) ?? { status: "running", findings: [] };
    if (event.type === "cycle") { entry.status = String(event.p.status ?? "running"); cap = num(event.p.max_cycles) || cap; }
    if (event.type === "finding") entry.findings.push(progressText(event));
    cycles.set(cycle, entry);
  }
  const latest = Math.max(task.loopCycles, ...cycles.keys());
  const [action, setAction] = useState<SteerKind | null>(null);
  const [text, setText] = useState("");
  const steer = useMutation({
    mutationFn: (kind: SteerKind) => steerAttempt(attemptId, kind, kind === "halt" ? { reason: text } : kind === "instruction" ? { text } : kind === "set_max_cycles" ? { n: Number(text) } : {}),
    onSuccess: () => setAction(null),
  });
  const titles: Record<SteerKind, string> = { pause: "Pause after this cycle", halt: "Halt loop", set_max_cycles: "Change cycle cap", instruction: "Add instruction" };
  const valid = action === "pause" || (action === "set_max_cycles" ? Number.isInteger(Number(text)) && Number(text) >= latest && Number(text) <= 20 : text.trim().length > 0);
  const openAction = (kind: SteerKind) => { steer.reset(); setText(kind === "set_max_cycles" ? String(cap + 1) : ""); setAction(kind); };

  return <section className="table-section" aria-labelledby="loop-heading">
    <div className="table-heading"><div><h2 id="loop-heading">Loop</h2><p>Verify-fix cycles{task.loopWith ? ` with ${task.loopWith}` : ""}. Steering is read by the agent at the next cycle boundary.</p></div>
      <span className="fetched-time">Cycle {latest} / {cap || "?"}</span></div>
    <ol className="loop-cycles">{[...cycles.entries()].filter(([cycle]) => cycle > 0).sort(([a], [b]) => a - b).map(([cycle, entry]) => <li key={cycle}>
      <span className={`state-badge ${entry.status === "pass" ? "state-succeeded" : entry.status === "fail" ? "state-failed" : "state-running"}`}>Cycle {cycle} · {entry.status}</span>
      {entry.findings.length ? <ul>{entry.findings.map((finding, index) => <li key={index}>{finding}</li>)}</ul> : null}
    </li>)}</ol>
    <div className="table-footer"><span>Controls</span><div>
      {(["pause", "halt", "set_max_cycles", "instruction"] as SteerKind[]).map((kind) => <button key={kind} type="button" className="secondary-button" onClick={() => openAction(kind)}>{titles[kind]}</button>)}
    </div></div>
    <ConfirmModal open={action !== null} title={action ? titles[action] : ""} confirmLabel="Send to agent" pending={steer.isPending} disabled={!valid}
      error={steer.isError ? errorText(steer.error, "The agent could not be steered. Please try again.") : undefined}
      onConfirm={() => action && steer.mutate(action)} onCancel={() => setAction(null)}>
      {action === "pause" ? <p>The agent finishes cycle {latest || 1} and waits.</p> : null}
      {action === "halt" ? <TextArea label="Reason" value={text} onChange={setText} placeholder="Why should this loop stop?" hint="Recorded with the halt." /> : null}
      {action === "instruction" ? <TextArea label="Instruction for the next cycle" value={text} onChange={setText} placeholder="Focus on the failing auth test first." /> : null}
      {action === "set_max_cycles" ? <div className="form-field"><label htmlFor="loop-cap">Maximum cycles</label>
        <input id="loop-cap" type="number" min={Math.max(1, latest)} max={20} value={text} onChange={(event) => setText(event.target.value)} /></div> : null}
    </ConfirmModal>
  </section>;
}

export function StagePanel({ task, principalId, mayControl, interactions, events, status, scope, runId }: {
  task: RunTask; principalId: string; mayControl: boolean; interactions: Interaction[]; events: WorkflowEvent[]; status: AgentStatus | null; scope: string; runId: string;
}) {
  const [tab, setTab] = useState<StageTab>("work");
  const tabIds = { work: useId(), terminal: useId() };
  const [state, setState] = useState<TerminalState | null>(null);
  const [link, setLink] = useState<TerminalLink>({ status: "connecting" });
  const [confirm, setConfirm] = useState<"take" | "hand" | null>(null);
  const attemptId = task.activeAttemptId;
  useEffect(() => { setState(null); setLink({ status: "connecting" }); }, [attemptId]);
  const inControl = state?.control === "human" && state.holder === principalId;
  const access = useQuery({ queryKey: ["attempt-control", scope, attemptId], queryFn: ({ signal }) => getAttemptControl(attemptId, signal), enabled: !!attemptId && mayControl });
  const canTakeOver = access.data?.canTakeOver === true;
  const control = useMutation({
    mutationFn: (kind: "take" | "hand") => kind === "take" ? takeOverAttempt(attemptId) : handBackAttempt(attemptId),
    onSuccess: () => setConfirm(null),
  });
  const controlLabel = link.status === "ended" ? link.message || "Session ended" : link.status !== "open" ? (link.status === "closed" ? "Disconnected" : "Connecting…")
    : !state ? "Connected" : state.control === "agent" ? "Agent running" : inControl ? "You have control" : `${state.holder ?? "Someone"} has control`;
  const tone = link.status !== "open" ? "control-off" : inControl ? "control-you" : state?.control === "human" ? "control-other" : "control-agent";
  const stageEvents: Progress[] = events.filter((event) => event.taskId === task.id).map((event) => {
    const p = event.kind === "attempt.progress" ? eventPayload(event) : {};
    return { id: event.id.toString(), attemptId: event.attemptId, at: event.occurredAt, kind: event.kind, type: typeof p.type === "string" ? p.type : "progress", p };
  });
  const interview = interactions.filter((item) => item.stage === task.key && item.kind === "interview_round");
  const loopEvents = stageEvents.filter((event) => event.kind === "attempt.progress" && (event.type === "cycle" || event.type === "finding"));
  const steerAttemptId = attemptId || stageEvents.at(-1)?.attemptId || "";
  // The work log follows the live attempt, or the latest one that reported.
  const logAttempt = attemptId || stageEvents.filter((event) => event.kind === "attempt.progress").at(-1)?.attemptId || "";
  const logEvents = stageEvents.filter((event) => event.attemptId === logAttempt);
  const plan = workLog(logEvents).plan;
  useEffect(() => { if (inControl) setTab("terminal"); }, [inControl]);
  const tabKeys = (event: KeyboardEvent) => {
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight" && event.key !== "Home" && event.key !== "End") return;
    event.preventDefault();
    const next: StageTab = event.key === "Home" ? "work" : event.key === "End" ? "terminal" : tab === "work" ? "terminal" : "work";
    setTab(next);
    document.getElementById(`${tabIds[next]}-tab`)?.focus();
  };

  return <>
    <section className="table-section" aria-labelledby="stage-heading">
      <div className="table-heading"><div><h2 id="stage-heading"><TerminalSquare size={15} aria-hidden="true" /> {task.key} <StatusPill status={status} /></h2>
        <p>{task.kind === "human_review" ? "Human review" : [task.kind.replaceAll("_", " "), task.harness, task.model, task.effort].filter(Boolean).join(" · ")} · {task.state.replaceAll("_", " ")}{logAttempt ? ` · Attempt ${logAttempt.slice(0, 8)}` : ""}</p></div>
        {attemptId ? <div className="control-bar">
          <span className={`control-pill ${tone}`} role="status" aria-live="polite">{controlLabel}</span>
          {inControl ? <button type="button" className="secondary-button" onClick={() => { control.reset(); setConfirm("hand"); }}>Hand back</button>
            : canTakeOver ? <button type="button" className="primary-button" disabled={!mayControl || link.status !== "open" || state?.control !== "agent"} onClick={() => { control.reset(); setConfirm("take"); }}><Hand size={15} aria-hidden="true" /> Take over</button> : null}
        </div> : null}</div>
      <div className="stage-tabs" role="tablist" aria-label={`Stage ${task.key} views`} onKeyDown={tabKeys}>
        {(["work", "terminal"] as StageTab[]).map((name) => <button key={name} type="button" role="tab" id={`${tabIds[name]}-tab`}
          aria-selected={tab === name} aria-controls={`${tabIds[name]}-panel`} tabIndex={tab === name ? 0 : -1} className="stage-tab" onClick={() => setTab(name)}>
          {name === "work" ? <>Work log{plan?.length ? <TasksBadge plan={plan} /> : null}</> : <>Terminal</>}
        </button>)}
      </div>
      <div role="tabpanel" id={`${tabIds.work}-panel`} aria-labelledby={`${tabIds.work}-tab`} hidden={tab !== "work"}>
        <WorkLog events={logEvents} />
      </div>
      {/* The terminal stays mounted while hidden so its socket, scrollback and control state survive tab switches. */}
      <div role="tabpanel" id={`${tabIds.terminal}-panel`} aria-labelledby={`${tabIds.terminal}-tab`} hidden={tab !== "terminal"}>
        {attemptId ? <AttemptTerminal key={attemptId} attemptId={attemptId} inControl={inControl} onState={setState} onLink={setLink} />
          : <div className="table-empty">This stage has no live attempt. Its terminal opens when the stage starts.</div>}
      </div>
    </section>
    <ConfirmModal open={confirm !== null} title={confirm === "take" ? "Take over this stage" : "Hand back to the agent"}
      confirmLabel={confirm === "take" ? "Take over" : "Hand back"} pending={control.isPending}
      error={control.isError ? errorText(control.error, "Control could not be changed. Please try again.") : undefined}
      onConfirm={() => confirm && control.mutate(confirm)} onCancel={() => setConfirm(null)}>
      <p>{confirm === "take"
        ? "The agent is interrupted and its session resumes in your terminal. The model API key will be visible to you in this session. Only you can type until you hand back. Your edits go through the same checks and review as the agent's."
        : "Your interactive session exits and the stage finishes from where you left it. Exit any editor or prompt first."}</p>
    </ConfirmModal>
    {interview.length ? <InterviewView items={interview} mayAnswer={mayControl} scope={scope} runId={runId} /> : null}
    {(task.loopWith || loopEvents.length) && steerAttemptId ? <LoopPanel attemptId={steerAttemptId} events={loopEvents} task={task} /> : null}
  </>;
}
