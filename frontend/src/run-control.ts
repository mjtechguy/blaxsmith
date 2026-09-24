// Run-control surface from docs/interactive-sessions.md over the generated Connect client.
// Human review uses the existing GetCurrentReview/DecideReview (workflow.ts); live updates use
// the existing run SSE (`interaction.*`, `attempt.progress`, `attempt.control` kinds).
import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { WorkflowService, type Interaction, type WorkflowEvent } from "./gen/blaxsmith/api/v1/workflow_pb";

export type { Interaction };

const client = createClient(WorkflowService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

// canTakeOver: takeover shows the model API key, so only org owners/admins or the
// owner of the attempt's personal model connection get the button.
export const getAttemptControl = (attemptId: string, signal?: AbortSignal) => client.getAttemptControl({ attemptId }, { signal });
export const takeOverAttempt = async (attemptId: string) => (await client.takeOverAttempt({ attemptId }, await csrf())).control;
export const handBackAttempt = async (attemptId: string) => (await client.handBackAttempt({ attemptId }, await csrf())).control;

export type TerminalState = { type: "state"; control: "agent" | "human"; holder: string | null; stage: string; attemptStatus: string };
export type TerminalFrame = TerminalState | { type: "exit"; code: number } | { type: "error"; message: string };

export const terminalUrl = (attemptId: string) =>
  `${window.location.protocol === "https:" ? "wss" : "ws"}://${window.location.host}/api/terminal/attempts/${encodeURIComponent(attemptId)}`;

export type SteerKind = "instruction" | "pause" | "halt" | "set_max_cycles";

export const listInteractions = async (runId: string, signal?: AbortSignal): Promise<Interaction[]> =>
  (await client.listInteractions({ runId }, { signal })).interactions;
export const answerInteraction = async (interactionId: string, optionIds: string[], text: string) =>
  client.answerInteraction({ interactionId, optionIds, text: text.trim() }, await csrf());
export const steerAttempt = async (attemptId: string, kind: SteerKind, value: { text?: string; reason?: string; n?: number } = {}) =>
  client.steerAttempt({ attemptId, kind, ...value }, await csrf());

// `attempt.progress` carries the raw `bx event` JSON in WorkflowEvent.payload_json.
// A missing or malformed payload renders as a generic progress line.
export function eventPayload(event: WorkflowEvent): Record<string, unknown> {
  try {
    const value: unknown = JSON.parse(event.payloadJson || "{}");
    return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
  } catch { return {}; }
}
