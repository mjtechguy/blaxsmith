// Pure view logic for the stage work log, status pills, and question-card keys.
// Shapes follow docs/interactive-sessions.md ("Agent activity"). Grouping and
// rollup ideas come from t3code (MIT, commit b2b43bef73): session-logic.ts and
// Sidebar.logic.ts. No code is copied.

export type Progress = { id: string; attemptId: string; at: string; kind: string; type: string; p: Record<string, unknown> };

export type ToolRow = { key: string; id: string; tool: string; input: string; status: string; durationMs?: number; truncated: boolean };
export type WorkEntry =
  | { kind: "tools"; key: string; items: ToolRow[] }
  | { kind: "command"; key: string; id: string; cmd: string; status: string; exit?: number; durationMs?: number; truncated: boolean }
  | { kind: "file"; key: string; id: string; path: string; change: string; status: string; added?: number; removed?: number }
  | { kind: "message"; key: string; id: string; text: string; truncated: boolean }
  | { kind: "note"; key: string; event: Progress };
export type PlanItem = { text: string; status: "pending" | "in_progress" | "completed" | "cancelled" };

const str = (value: unknown) => typeof value === "string" ? value : "";
const opt = (value: unknown) => typeof value === "number" && Number.isFinite(value) ? value : undefined;
const workTypes = new Set(["tool.started", "tool.completed", "command", "file.changed", "plan.updated", "assistant.message"]);
export const isWorkEvent = (event: Progress) => event.kind === "attempt.progress" && workTypes.has(event.type);

// family + id identifies one item: a later record replaces the earlier one in place.
const itemKey = (event: Progress) => `${event.attemptId}\u0000${event.type.startsWith("tool.") ? "tool" : event.type}\u0000${str(event.p.id)}`;

// workLog folds progress events (oldest first) into timeline entries. Consecutive
// tool calls become one collapsible group; the latest plan is returned separately.
export function workLog(events: Progress[]): { entries: WorkEntry[]; plan: PlanItem[] | null } {
  const latest = new Map<string, Progress>();
  const order: string[] = [];
  let plan: PlanItem[] | null = null;
  for (const event of events) {
    if (event.kind !== "attempt.progress") continue;
    if (event.type === "plan.updated") { plan = planItems(event.p.items); continue; }
    const key = isWorkEvent(event) ? itemKey(event) : `note\u0000${event.id}`;
    if (!latest.has(key)) order.push(key);
    latest.set(key, event);
  }
  const entries: WorkEntry[] = [];
  for (const key of order) {
    const event = latest.get(key)!;
    const p = event.p;
    const truncated = p.truncated === true;
    const id = str(p.id);
    switch (event.type) {
      case "tool.started":
      case "tool.completed": {
        const row: ToolRow = { key, id, tool: str(p.tool) || "tool", input: str(p.input), status: str(p.status) || "running", durationMs: opt(p.duration_ms), truncated };
        const last = entries.at(-1);
        if (last?.kind === "tools") last.items.push(row);
        else entries.push({ kind: "tools", key, items: [row] });
        break;
      }
      case "command":
        entries.push({ kind: "command", key, id, cmd: str(p.cmd), status: str(p.status) || "running", exit: opt(p.exit), durationMs: opt(p.duration_ms), truncated });
        break;
      case "file.changed":
        entries.push({ kind: "file", key, id, path: str(p.path), change: str(p.kind), status: str(p.status) || "completed", added: opt(p.added), removed: opt(p.removed) });
        break;
      case "assistant.message":
        entries.push({ kind: "message", key, id, text: str(p.text), truncated });
        break;
      default:
        entries.push({ kind: "note", key, event });
    }
  }
  return { entries, plan };
}

function planItems(raw: unknown): PlanItem[] {
  if (!Array.isArray(raw)) return [];
  return raw.flatMap((item) => {
    if (!item || typeof item !== "object") return [];
    const { text, status } = item as Record<string, unknown>;
    const s = status === "completed" || status === "in_progress" || status === "cancelled" ? status : "pending";
    return typeof text === "string" && text ? [{ text, status: s }] : [];
  });
}

// groupSummary is the collapsed label of a tool group, e.g. "3 tool calls · 1 failed".
export function groupSummary(items: ToolRow[]): string {
  const failed = items.filter((item) => item.status === "failed").length;
  const running = items.filter((item) => item.status === "running").length;
  return [`${items.length} tool call${items.length === 1 ? "" : "s"}`, running ? `${running} running` : "", failed ? `${failed} failed` : ""].filter(Boolean).join(" · ");
}

export function planProgress(plan: PlanItem[]): { done: number; total: number; current?: string } {
  const active = plan.filter((item) => item.status !== "cancelled");
  return { done: active.filter((item) => item.status === "completed").length, total: active.length, current: active.find((item) => item.status === "in_progress")?.text };
}

export const formatDuration = (ms?: number) => ms === undefined ? "" : ms < 1_000 ? `${ms} ms` : ms < 60_000 ? `${(ms / 1_000).toFixed(1)} s` : `${Math.floor(ms / 60_000)} m ${Math.round((ms % 60_000) / 1_000)} s`;

// Status pills, highest priority first.
export const statusOrder = ["needs_approval", "awaiting_input", "working", "plan_ready", "failed", "done"] as const;
export type AgentStatus = typeof statusOrder[number];
export const statusLabels: Record<AgentStatus, string> = {
  needs_approval: "Needs approval", awaiting_input: "Awaiting input", working: "Working", plan_ready: "Plan ready", failed: "Failed", done: "Done",
};
export const needsYou = (status: AgentStatus | null) => status === "needs_approval" || status === "awaiting_input";

export type StageLike = { key: string; kind: string; state: string; dependsOn: string[]; activeAttemptId?: string };
export type OpenItem = { stage: string; kind: string; state: string };

// stageStatus derives one stage's pill from its task row and the run's interactions.
// reviewWaiting marks a presented, undecided final review (the human_review stage).
export function stageStatus(task: StageLike, interactions: OpenItem[], tasks: StageLike[], reviewWaiting = false): AgentStatus | null {
  const open = interactions.filter((item) => item.state === "open" && item.stage === task.key);
  if (open.some((item) => item.kind === "approval") || (task.kind === "human_review" && reviewWaiting)) return "needs_approval";
  if (open.length || task.state === "escalated") return "awaiting_input";
  if (["reserved", "starting", "running", "reconciling"].includes(task.state)) return "working";
  if (task.state === "succeeded") {
    const waiting = tasks.some((other) => other.dependsOn.includes(task.key) && other.state === "pending");
    return task.kind === "plan" && waiting ? "plan_ready" : "done";
  }
  if (task.state === "blocked") return "failed";
  return null;
}

// rollup picks the most urgent status (Needs approval > … > Failed > Done).
export function rollup(statuses: Array<AgentStatus | null | undefined>): AgentStatus | null {
  let best: AgentStatus | null = null;
  for (const status of statuses) {
    if (status && (best === null || statusOrder.indexOf(status) < statusOrder.indexOf(best))) best = status;
  }
  return best;
}

// runStatus rolls a run's stages up, falling back to the run state when no stage says more.
export function runStatus(runState: string, tasks: StageLike[], interactions: OpenItem[], reviewWaiting = false): AgentStatus | null {
  const stages = rollup(tasks.map((task) => stageStatus(task, interactions, tasks, reviewWaiting)));
  if (stages && stages !== "done") return stages;
  if (runState === "succeeded") return "done";
  if (runState === "failed") return "failed";
  return runState === "active" && interactions.some((item) => item.state === "open") ? "awaiting_input" : stages;
}

// attentionTitle is the browser tab title: "(2) Blaxsmith" while things need you.
export const attentionTitle = (count: number, base = "Blaxsmith") => count > 0 ? `(${count > 99 ? "99+" : count}) ${base}` : base;

// approvalChoices finds the options an approval's Approve / Decline buttons answer with.
// Other options (e.g. "Revise") stay selectable in the list.
type OptionLike = { id: string; label: string; recommended?: boolean };
const approveWords = /^(approve|approved|go|yes|accept|ok|proceed|continue)$/i;
const declineWords = /^(decline|declined|reject|no|deny|cancel|stop|halt)$/i;
export function approvalChoices<T extends OptionLike>(options: T[]): { approve?: T; decline?: T } {
  const match = (words: RegExp) => options.find((option) => words.test(option.id) || words.test(option.label.trim()));
  const approve = match(approveWords) ?? options.find((option) => option.recommended);
  const decline = match(declineWords);
  return { approve, decline: decline !== approve ? decline : undefined };
}

// Question-card keyboard model: 1-9 pick options, Enter sends, Escape cancels a
// free-text edit (Shift+Enter is a newline there). Digits typed into the text
// box stay text. Returns what a key press means, or null to let it through.
export type CardKey = { action: "select"; index: number } | { action: "submit" } | { action: "cancel_text" } | null;
export function cardKey(key: string, optionCount: number, inText: boolean, mods: { shift?: boolean; ctrl?: boolean; meta?: boolean; alt?: boolean; composing?: boolean } = {}): CardKey {
  if (mods.composing || mods.alt) return null;
  if (key === "Enter" && !mods.shift) return { action: "submit" };
  if (inText) return key === "Escape" ? { action: "cancel_text" } : null;
  if (mods.ctrl || mods.meta || mods.shift) return null;
  if (/^[1-9]$/.test(key) && Number(key) <= optionCount) return { action: "select", index: Number(key) - 1 };
  return null;
}
