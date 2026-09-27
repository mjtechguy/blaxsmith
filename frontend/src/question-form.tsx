import { useId, useRef, useState, type KeyboardEvent, type Ref } from "react";
import { useMutation } from "@tanstack/react-query";
import { Code, ConnectError } from "@connectrpc/connect";
import { Check, FileText, X } from "lucide-react";
import type { Interaction } from "./gen/blaxsmith/api/v1/workflow_pb";
import { Markdown } from "./markdown";
import { approvalChoices, cardKey } from "./agent-view";
const kindLabels: Record<string, string> = { question: "Question", approval: "Approval gate", escalation: "Escalation", interview_round: "Interview" };
const kindLabel = (kind: string) => kindLabels[kind] ?? kind.replaceAll("_", " ");

export function errorText(cause: unknown, fallback: string) {
  const code = ConnectError.from(cause).code;
  return code === Code.FailedPrecondition ? "This changed since you loaded it. Check the latest state and try again."
    : code === Code.PermissionDenied ? "Your session is not allowed to do this." : fallback;
}

export function TextArea({ label, value, onChange, placeholder, hint, inputRef, describedBy }: {
  label: string; value: string; onChange: (value: string) => void; placeholder: string; hint?: string;
  inputRef?: Ref<HTMLTextAreaElement>; describedBy?: string;
}) {
  const id = useId();
  return <div className="review-feedback-input"><label htmlFor={id}>{label}</label>
    <textarea ref={inputRef} id={id} value={value} rows={4} maxLength={4000} placeholder={placeholder} aria-describedby={describedBy} onChange={(event) => onChange(event.target.value)} />
    {hint ? <small>{hint}</small> : null}</div>;
}

// The same accessible question form serves goal intake and live run interactions.
export function QuestionForm({ item, mayAnswer, onAnswer }: { item: Interaction; mayAnswer: boolean; onAnswer: (reply: { optionIds: string[]; text: string }) => Promise<unknown> }) {
  const [selected, setSelected] = useState<string[]>(() => item.answer?.optionIds ?? item.options.filter((option) => option.recommended && !item.multiSelect).map((option) => option.id).slice(0, 1));
  const [text, setText] = useState(item.answer?.text || "");
  const [error, setError] = useState("");
  const name = useId();
  const hintId = useId();
  const optionRefs = useRef<Array<HTMLInputElement | null>>([]);
  const textRef = useRef<HTMLTextAreaElement>(null);
  const answer = useMutation({
    mutationFn: onAnswer,
    onSuccess: () => setError(""),
    onError: (cause) => setError(errorText(cause, "The answer could not be sent. Please try again.")),
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
      <span className="ix-stage">{item.stage ? `Stage ${item.stage}` : "Goal planning"}</span>
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
        value={text} onChange={setText} placeholder="Type your reply." /> : null}
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
