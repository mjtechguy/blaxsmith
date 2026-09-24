// One sign-in state machine for every credential flow: an API key being
// validated, the Codex device code, and the GitHub OAuth redirect. The idea
// follows t3code's provider setup phases (contracts/src/providerSetup.ts);
// the code is Blaxsmith's own.
//
//   idle ─start→ starting ─started→ waiting ─verify→ verifying ─succeed→ succeeded
//                  │ ╰────────verify──────────────────╯    │
//                  ╰─ fail / cancel (from any busy phase) ─┴→ failed | cancelled ─retry→ idle
//
// Transitions not in the diagram leave the state unchanged, so a late poll
// result cannot resurrect a cancelled flow.

export type SignInPhase = "idle" | "starting" | "waiting" | "verifying" | "succeeded" | "failed" | "cancelled";

export type DeviceCode = { verificationUrl: string; userCode: string; expiresAt: string };

export type SignInState = { phase: SignInPhase; attempt: number; device?: DeviceCode; error?: string };

export type SignInEvent =
  | { type: "start" }
  | { type: "started"; device?: DeviceCode }
  | { type: "verify" }
  | { type: "succeed" }
  | { type: "fail"; error: string }
  | { type: "expire" }
  | { type: "cancel" }
  | { type: "retry" };

export const initialSignIn: SignInState = { phase: "idle", attempt: 0 };

export const isBusy = (phase: SignInPhase) => phase === "starting" || phase === "waiting" || phase === "verifying";

export function signInReducer(state: SignInState, event: SignInEvent): SignInState {
  const { phase } = state;
  switch (event.type) {
    case "start":
      return phase === "idle" ? { phase: "starting", attempt: state.attempt + 1 } : state;
    case "started":
      return phase === "starting" ? { ...state, phase: "waiting", device: event.device } : state;
    case "verify":
      return phase === "starting" || phase === "waiting" ? { ...state, phase: "verifying" } : state;
    case "succeed":
      return isBusy(phase) ? { ...state, phase: "succeeded", error: undefined } : state;
    case "fail":
      return isBusy(phase) ? { ...state, phase: "failed", error: event.error } : state;
    case "expire":
      return phase === "waiting" ? { ...state, phase: "failed", error: "The code expired before it was approved." } : state;
    case "cancel":
      return isBusy(phase) ? { ...state, phase: "cancelled" } : state;
    case "retry":
      return phase === "failed" || phase === "cancelled" ? { phase: "idle", attempt: state.attempt } : state;
  }
}

export function secondsLeft(expiresAt: string, now = Date.now()): number {
  const at = Date.parse(expiresAt);
  return Number.isNaN(at) ? 0 : Math.max(0, Math.ceil((at - now) / 1000));
}

export function formatCountdown(seconds: number): string {
  return `${Math.floor(seconds / 60)}:${String(seconds % 60).padStart(2, "0")}`;
}
