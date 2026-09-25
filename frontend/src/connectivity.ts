// Backend reachability, kept apart from authentication. A restarting pod, a
// rollout, a proxy 502/503/504, a Connect Unavailable, or a dropped network is
// "reconnecting": the session is kept, a non-blocking banner shows, and work
// resumes once the server answers again. Only Unauthenticated ends a session
// (see auth.ts).
import { Code, ConnectError } from "@connectrpc/connect";

export function isUnavailable(error: unknown): boolean {
  if (error instanceof TypeError) return true; // fetch could not reach the server
  const e = ConnectError.from(error);
  if (e.code === Code.Unavailable || e.code === Code.DeadlineExceeded) return true;
  return e.code === Code.Unknown && e.cause instanceof TypeError;
}

// Capped exponential backoff with "equal jitter": half the ceiling is fixed,
// half random, so a fleet of tabs does not reconnect in lockstep.
export function backoffDelay(attempt: number, random: () => number = Math.random, base = 1_000, cap = 30_000): number {
  const ceiling = Math.min(cap, base * 2 ** Math.min(Math.max(attempt, 0), 16));
  return Math.round(ceiling / 2 + random() * (ceiling / 2));
}

export type ConnectionState = "online" | "reconnecting";

let state: ConnectionState = "online";
let attempt = 0;
let timer: ReturnType<typeof setTimeout> | undefined;
let probe: (() => Promise<unknown>) | undefined;
const listeners = new Set<() => void>();
const recovered = new Set<() => void>();

export const connectionState = (): ConnectionState => state;

export function subscribeConnection(listener: () => void): () => void {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

// Runs once each time the server answers again after an outage.
export function onReconnected(listener: () => void): () => void {
  recovered.add(listener);
  return () => { recovered.delete(listener); };
}

// The cheapest call that proves the server answers; any reply but an
// unavailability (even Unauthenticated) means it is back.
export function setConnectionProbe(next: () => Promise<unknown>): void {
  probe = next;
}

function emit() {
  for (const listener of [...listeners]) listener();
}

export function reportUnavailable(): void {
  if (state === "reconnecting") return;
  state = "reconnecting";
  attempt = 0;
  emit();
  schedule(backoffDelay(0));
}

export function reportReachable(): void {
  if (state === "online") return;
  state = "online";
  clearTimeout(timer);
  timer = undefined;
  emit();
  for (const listener of [...recovered]) listener();
}

function schedule(delay: number) {
  clearTimeout(timer);
  timer = setTimeout(() => void probeNow(), delay);
}

// Probes at once (the tab became visible, or the browser came back online);
// otherwise the backoff schedule probes on its own.
export async function probeNow(): Promise<void> {
  if (state === "online") return;
  clearTimeout(timer);
  try {
    await probe?.();
    reportReachable();
  } catch (error) {
    if (!isUnavailable(error)) { reportReachable(); return; }
    if (state === "reconnecting") schedule(backoffDelay(++attempt));
  }
}
