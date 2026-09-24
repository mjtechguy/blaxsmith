import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";
import type { QueryClient } from "@tanstack/react-query";
import { AuthService, type SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";

export const sessionQueryKey = ["browser-session"] as const;
export const isPublicCatalogRoute = (pathname: string): boolean => pathname === "/tools";
// Account setup/reset links are redeemed without a session.
export const isAccountLinkRoute = (pathname: string): boolean => /^\/setup\/[^/]+$/.test(pathname);
const workspaceQuery = (query: { queryKey: readonly unknown[] }) => query.queryKey[0] !== sessionQueryKey[0];
let sessionChannel: BroadcastChannel | undefined;

function channel(): BroadcastChannel {
  if (typeof BroadcastChannel === "undefined") throw new Error("Secure session coordination is unavailable");
  return sessionChannel ??= new BroadcastChannel("blaxsmith-session");
}

export function onOtherTabSessionChange(callback: () => void): () => void {
  if (typeof BroadcastChannel === "undefined") return () => {};
  const listener = () => callback();
  channel().addEventListener("message", listener);
  return () => channel().removeEventListener("message", listener);
}

export function announceSessionChange(): void {
  channel().postMessage("changed");
}

export async function clearWorkspaceCache(queryClient: QueryClient): Promise<void> {
  await queryClient.cancelQueries({ predicate: workspaceQuery });
  queryClient.removeQueries({ predicate: workspaceQuery });
}

export const browserTransport = createConnectTransport({
  baseUrl: `${window.location.origin}/api`,
  fetch: (input, init) => fetch(input, { ...init, credentials: "same-origin" }),
});
const client = createClient(AuthService, browserTransport);

// Shared cookie rotation must be serialized across tabs. A later tab checks
// the new access cookie before it tries to use the old refresh cookie.
function withSessionLock<T>(action: (signal: AbortSignal) => Promise<T>, upstream?: AbortSignal): Promise<T> {
  if (!navigator.locks || typeof BroadcastChannel === "undefined") throw new Error("Secure session coordination is unavailable");
  const controller = new AbortController();
  const abort = () => controller.abort();
  upstream?.addEventListener("abort", abort, { once: true });
  if (upstream?.aborted) abort();
  const timer = window.setTimeout(abort, 20_000);
  return navigator.locks.request("blaxsmith-session", { mode: "exclusive", signal: controller.signal },
    () => action(controller.signal)).finally(() => {
      window.clearTimeout(timer);
      upstream?.removeEventListener("abort", abort);
    });
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

export async function currentSession(signal?: AbortSignal): Promise<SessionIdentity | null> {
  if (!navigator.locks || typeof BroadcastChannel === "undefined") throw new Error("Secure session coordination is unavailable");
  try {
    return identity((await client.currentSession({}, { signal, timeoutMs: 10_000 })).session);
  } catch (error) {
    if (ConnectError.from(error).code !== Code.Unauthenticated) throw error;
  }
  return withSessionLock(async (lockSignal) => {
    try {
      return identity((await client.currentSession({}, { signal: lockSignal })).session);
    } catch (error) {
      if (ConnectError.from(error).code !== Code.Unauthenticated) throw error;
    }
    const token = await csrf(lockSignal);
    try {
      return identity((await client.refreshSession({}, { signal: lockSignal, headers: { "X-Blaxsmith-CSRF": token } })).session);
    } catch (error) {
      if (ConnectError.from(error).code === Code.Unauthenticated) return null;
      throw error;
    }
  }, signal);
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
