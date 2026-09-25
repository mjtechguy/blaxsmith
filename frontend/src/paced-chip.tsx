// A stage queued for model gateway headroom (docs/model-gateway-plan.md §5):
// the reason and a live reset countdown, shown on the stage chip and heading.
import { Hourglass } from "lucide-react";
import { countdown, useNow } from "./gateway";

export function PacedChip({ reason, resetsAt }: { reason: string; resetsAt: string }) {
  const now = useNow();
  if (!reason) return null;
  const left = countdown(resetsAt, now);
  const text = left ? `${reason}, resets in ${left}` : reason;
  return <span className="paced-chip" role="status" title={text}><Hourglass size={11} aria-hidden="true" />{text}</span>;
}
