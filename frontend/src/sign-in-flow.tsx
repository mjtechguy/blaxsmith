import { useEffect, useReducer, useState, type ReactNode } from "react";
import { Check, Copy, ExternalLink, RefreshCw, RotateCcw, X } from "lucide-react";
import { formatCountdown, initialSignIn, secondsLeft, signInReducer, type SignInState } from "./sign-in";

export function useSignIn() {
  return useReducer(signInReducer, initialSignIn);
}

function CopyButton({ text, label }: { text: string; label: string }) {
  const [copied, setCopied] = useState(false);
  useEffect(() => { if (!copied) return; const t = window.setTimeout(() => setCopied(false), 2_000); return () => window.clearTimeout(t); }, [copied]);
  return <button type="button" className="text-action" onClick={() => { void navigator.clipboard?.writeText(text).then(() => setCopied(true), () => undefined); }}>
    {copied ? <Check size={13} aria-hidden="true" /> : <Copy size={13} aria-hidden="true" />} {copied ? "Copied" : label}</button>;
}

// Device-code expiry countdown; calls onExpire once when it reaches zero.
function Countdown({ expiresAt, onExpire }: { expiresAt: string; onExpire: () => void }) {
  const [left, setLeft] = useState(() => secondsLeft(expiresAt));
  useEffect(() => {
    const timer = window.setInterval(() => setLeft(secondsLeft(expiresAt)), 1_000);
    return () => window.clearInterval(timer);
  }, [expiresAt]);
  useEffect(() => { if (left === 0) onExpire(); }, [left, onExpire]);
  return <span>Expires in <time dateTime={expiresAt}>{formatCountdown(left)}</time></span>;
}

// The one presentation of a sign-in's progress. idle renders nothing: the
// caller shows its start control (a form or a button) then.
export function SignInStatus({ state, labels, onCancel, onRetry, onExpire, done }: {
  state: SignInState;
  labels: { starting: string; waiting?: string; verifying: string; succeeded: string };
  onCancel?: () => void; onRetry: () => void; onExpire?: () => void; done?: ReactNode;
}) {
  const cancel = onCancel ? <button type="button" className="secondary-button" onClick={onCancel}><X size={15} aria-hidden="true" /> Cancel</button> : null;
  switch (state.phase) {
    case "idle":
      return null;
    case "starting":
    case "verifying":
      return <div className="sign-in-status" role="status" aria-live="polite">
        <p><RefreshCw size={14} className="spin" aria-hidden="true" /> {state.phase === "starting" ? labels.starting : labels.verifying}</p>
        {cancel}
      </div>;
    case "waiting":
      return <div className="sign-in-status device-login" role="status" aria-live="polite">
        {state.device ? <>
          <p>1. Open <a className="text-action" href={state.device.verificationUrl} target="_blank" rel="noreferrer">{state.device.verificationUrl} <ExternalLink size={13} aria-hidden="true" /></a> and sign in. <CopyButton text={state.device.verificationUrl} label="Copy link" /></p>
          <p>2. Enter this one-time code:</p>
          <p className="sign-in-code"><code className="device-code">{state.device.userCode}</code> <CopyButton text={state.device.userCode} label="Copy code" /></p>
          <p className="admin-note"><RefreshCw size={12} className="spin" aria-hidden="true" /> {labels.waiting || "Waiting for approval…"}{" "}
            {state.device.expiresAt ? <Countdown expiresAt={state.device.expiresAt} onExpire={onExpire ?? (() => undefined)} /> : null}</p>
        </> : <p><RefreshCw size={14} className="spin" aria-hidden="true" /> {labels.waiting || "Waiting…"}</p>}
        {cancel}
      </div>;
    case "succeeded":
      return <div className="notice" role="status"><strong><Check size={14} aria-hidden="true" /> {labels.succeeded}</strong> {done}</div>;
    case "failed":
    case "cancelled":
      return <div className="notice" role={state.phase === "failed" ? "alert" : "status"}>
        <strong>{state.phase === "failed" ? "Sign-in failed." : "Sign-in cancelled."}</strong> {state.phase === "failed" ? state.error : null}{" "}
        <button type="button" className="text-action" onClick={onRetry}><RotateCcw size={13} aria-hidden="true" /> Try again</button>
      </div>;
  }
}
