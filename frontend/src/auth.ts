import { Code, ConnectError, createClient, type Interceptor } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import type { QueryClient } from "@tanstack/react-query";
import { AuthService, type SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";
import { isUnavailable, reportReachable, reportUnavailable, setConnectionProbe } from "./connectivity";

export const sessionQueryKey = ["browser-session"] as const;
export const isPublicCatalogRoute = (pathname: string): boolean => pathname === "/tools";
// Account setup/reset links are redeemed without a session.
export const isAccountLinkRoute = (pathname: string): boolean => /^\/setup\/[^/]+$/.test(pathname);
const workspaceQuery = (query: { queryKey: readonly unknown[] }) => query.queryKey[0] !== sessionQueryKey[0];
let sessionChannel: BroadcastChannel | undefined;

// Sign-in and sign-out in one tab reset the others. Without BroadcastChannel
// other tabs notice on their next session check instead.
function channel(): BroadcastChannel | undefined {
  if (typeof BroadcastChannel === "undefined") return undefined;
  return sessionChannel ??= new BroadcastChannel("blaxsmith-session");
}

export function onOtherTabSessionChange(callback: () => void): () => void {
  const shared = channel();
  if (!shared) return () => {};
  const listener = () => callback();
  shared.addEventListener("message", listener);
  return () => shared.removeEventListener("message", listener);
}

export function announceSessionChange(): void {
  channel()?.postMessage("changed");
}

export async function clearWorkspaceCache(queryClient: QueryClient): Promise<void> {
  await queryClient.cancelQueries({ predicate: workspaceQuery });
  queryClient.removeQueries({ predicate: workspaceQuery });
}

const baseUrl = () => `${window.location.origin}/api`;
const sameOriginFetch: typeof fetch = (input, init) => fetch(input, { ...init, credentials: "same-origin" });

// Every answer from the server, even an error, proves it is reachable.
function observe(error: unknown, signal: AbortSignal) {
  if (isUnavailable(error)) { if (!signal.aborted) reportUnavailable(); return; }
  if (ConnectError.from(error).code !== Code.Canceled) reportReachable();
}

const reachability: Interceptor = (next) => async (req) => {
  try {
    const response = await next(req);
    reportReachable();
    return response;
  } catch (error) {
    observe(error, req.signal);
    throw error;
  }
};

// AuthService calls manage the session themselves, so they only observe reachability.
const client = createClient(AuthService, createConnectTransport({ baseUrl: baseUrl(), fetch: sameOriginFetch, interceptors: [reachability] }));
setConnectionProbe(() => client.currentSession({}, { timeoutMs: 5_000 }));

let expired: (() => void) | undefined;

// Registers what happens when the session has really ended (a refresh was
// refused): the app clears the session, and the root layout routes to
// /login?next=<current path>.
export function onSessionExpired(handler: () => void): () => void {
  expired = handler;
  return () => { if (expired === handler) expired = undefined; };
}

// An RPC answered Unauthenticated: the access cookie expired or the server
// restarted between refreshes. Refresh once through the refresh cookie and
// retry the original request once; a refused refresh ends the session.
// Unavailability never does: it is reported and the error is left to the
// query/mutation retry policy.
const resilience: Interceptor = (next) => async (req) => {
  try {
    const response = await next(req);
    reportReachable();
    return response;
  } catch (error) {
    observe(error, req.signal);
    if (req.stream || ConnectError.from(error).code !== Code.Unauthenticated) throw error;
    const session = await renewSession(req.signal);
    if (!session) {
      expired?.();
      throw error;
    }
    return reachability(next)(req);
  }
};

export const browserTransport = createConnectTransport({ baseUrl: baseUrl(), fetch: sameOriginFetch, interceptors: [resilience] });

// Shared cookie rotation is serialized across tabs with the Web Locks API; a
// later tab rechecks the access cookie before it tries the refresh cookie, so
// only one tab refreshes. Without Web Locks, refreshes are serialized within
// the tab, and the server's refresh grace window absorbs a cross-tab race.
let localLock: Promise<unknown> = Promise.resolve();
function withSessionLock<T>(action: (signal: AbortSignal) => Promise<T>, upstream?: AbortSignal): Promise<T> {
  const controller = new AbortController();
  const abort = () => controller.abort();
  upstream?.addEventListener("abort", abort, { once: true });
  if (upstream?.aborted) abort();
  const timer = window.setTimeout(abort, 20_000);
  const cleanup = () => {
    window.clearTimeout(timer);
    upstream?.removeEventListener("abort", abort);
  };
  if (typeof navigator !== "undefined" && navigator.locks) {
    return navigator.locks.request("blaxsmith-session", { mode: "exclusive", signal: controller.signal },
      () => action(controller.signal)).finally(cleanup);
  }
  const run = localLock.then(() => action(controller.signal));
  localLock = run.catch(() => undefined);
  return run.finally(cleanup);
}

function identity(session?: SessionIdentity): SessionIdentity {
  if (!session?.organizationId || !session.principalId || !session.accessExpiresAt) {
    throw new Error("Session response is incomplete");
  }
  return session;
}

async function csrf(signal?: AbortSignal): Promise<string> {
  const response = await client.getCsrf({}, { signal });
  if (!response.token) throw new Error("CSRF response is incomplete");
  return response.token;
}

export const csrfToken = csrf;

// Refresh this long before the access token expires, so requests rarely see
// an expired cookie at all.
export const REFRESH_AHEAD_MS = 90_000;
const expiresSoon = (session: SessionIdentity) => Date.parse(session.accessExpiresAt) - Date.now() < REFRESH_AHEAD_MS;

async function current(signal: AbortSignal | undefined, timeoutMs?: number): Promise<SessionIdentity | null> {
  try {
    return identity((await client.currentSession({}, { signal, timeoutMs })).session);
  } catch (error) {
    if (ConnectError.from(error).code !== Code.Unauthenticated) throw error;
  }
  return null;
}

let renewal: Promise<SessionIdentity | null> | undefined;

// Refreshes through the refresh cookie under the session lock, unless another
// tab already did. Concurrent callers in this tab share one attempt. Returns
// null only when the server refuses the refresh (the session has ended);
// unavailability throws.
function renewSession(signal?: AbortSignal): Promise<SessionIdentity | null> {
  renewal ??= withSessionLock(async (lockSignal) => {
    const now = await current(lockSignal);
    if (now && !expiresSoon(now)) return now;
    const token = await csrf(lockSignal);
    try {
      return identity((await client.refreshSession({}, { signal: lockSignal, headers: { "X-Blaxsmith-CSRF": token } })).session);
    } catch (error) {
      if (ConnectError.from(error).code === Code.Unauthenticated) return null;
      throw error;
    }
  }).finally(() => { renewal = undefined; });
  const shared = renewal;
  if (!signal) return shared;
  return new Promise((resolve, reject) => {
    const abort = () => reject(ConnectError.from(signal.reason ?? new Error("aborted"), Code.Canceled));
    if (signal.aborted) abort();
    signal.addEventListener("abort", abort, { once: true });
    shared.then(resolve, reject).finally(() => signal.removeEventListener("abort", abort));
  });
}

// The signed-in identity, renewed silently when the access token has expired
// or expires within REFRESH_AHEAD_MS. null means signed out.
export async function currentSession(signal?: AbortSignal): Promise<SessionIdentity | null> {
  const now = await current(signal, 10_000);
  if (now && !expiresSoon(now)) return now;
  return renewSession(signal);
}

// Sign-in is by email. An account from before emails may enter its username
// here once; that session is flagged emailRequired and routed to set one.
export async function loginLocal(organizationSlug: string, email: string, password: string): Promise<SessionIdentity> {
  return withSessionLock(async (signal) => {
    const token = await csrf(signal);
    return identity((await client.loginLocal({ organizationSlug, email, password },
      { signal, headers: { "X-Blaxsmith-CSRF": token } })).session);
  });
}

export async function logout(): Promise<void> {
  await withSessionLock(async (signal) => {
    const token = await csrf(signal);
    await client.logout({}, { signal, headers: { "X-Blaxsmith-CSRF": token } });
  });
}

// The routed page a one-time legacy session must finish before anything else.
export const emailSetupPath = "/me/email";
export const needsOrganization = (error: unknown) => ConnectError.from(error).code === Code.FailedPrecondition;

export function loginError(error: unknown): string {
  switch (ConnectError.from(error).code) {
    case Code.Unauthenticated: return "That email and password were not accepted.";
    case Code.FailedPrecondition: return "Your account belongs to more than one organization. Enter the one to sign in to.";
    case Code.ResourceExhausted: return "Too many attempts. Please try again later.";
    default: return "Sign-in is unavailable. Please try again.";
  }
}
