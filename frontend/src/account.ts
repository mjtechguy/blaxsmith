// Account client for the user menu, Account settings, and the set-email page.
// AccountService acts only on the signed-in principal; mutations carry CSRF.
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { useQuery } from "@tanstack/react-query";
import { browserTransport, csrfToken, currentSession, sessionQueryKey } from "./auth";
import { AccountService } from "./gen/blaxsmith/api/v1/account_pb";

export const accountActions = true;

const client = createClient(AccountService, browserTransport);
const mutation = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

export const profileKey = (scope: string) => ["my-profile", scope] as const;
export const mySessionsKey = (scope: string) => ["my-sessions", scope] as const;

export const getMyProfile = async (signal?: AbortSignal) => (await client.getMyProfile({}, { signal })).profile;
export const listMySessions = async (signal?: AbortSignal) => (await client.listMySessions({}, { signal })).sessions;

// Send only what changes; an email change also needs the current password.
export async function updateMyProfile(change: { displayName?: string; email?: string; currentPassword?: string }) {
  return client.updateMyProfile({ displayName: change.displayName, email: change.email, currentPassword: change.currentPassword ?? "" }, await mutation());
}
export async function changeMyPassword(currentPassword: string, newPassword: string) {
  return client.changeMyPassword({ currentPassword, newPassword }, await mutation());
}
export async function revokeMySession(sessionId: string) {
  return client.revokeMySession({ sessionId }, await mutation());
}
export async function revokeMyOtherSessions() {
  return client.revokeMyOtherSessions({}, await mutation());
}

// Mirrors the server: 12–1024 bytes of UTF-8, the same rule as account setup.
export const passwordBytes = (value: string) => new TextEncoder().encode(value).length;
export const passwordProblem = (value: string) => passwordBytes(value) < 12 ? "Use at least 12 characters."
  : passwordBytes(value) > 1024 ? "Use at most 1024 characters." : undefined;
// A light client check; the server's rule is authoritative.
export const emailProblem = (value: string) => /^[^\s@<>()"]+@[^\s@<>()"]+\.[^\s@<>()".]+$/.test(value.trim()) && value.trim().length <= 254
  ? undefined : "Enter an email address like name@example.com.";

export function accountError(cause: unknown, fallback = "The change could not be saved. Please try again."): string {
  const error = ConnectError.from(cause);
  switch (error.code) {
    case Code.PermissionDenied: return error.rawMessage.includes("current password") ? "Your current password is not correct."
      : "This page's security check failed. Reload the page and try again.";
    case Code.AlreadyExists: return "That email is already used by another account.";
    case Code.InvalidArgument: return error.rawMessage.includes("password") ? "Use a password of 12–1024 characters."
      : error.rawMessage.includes("email") ? "Enter a valid email address." : "Check the details and try again.";
    case Code.ResourceExhausted: return "Too many attempts. Wait a few minutes and try again.";
    case Code.FailedPrecondition: return error.rawMessage.includes("sign out") ? "Use Sign out to end the session you are using."
      : "Set your email before changing anything else.";
    case Code.NotFound: return "That session has already ended.";
    case Code.Unauthenticated: return "Your session ended. Sign in again and retry.";
    default: return fallback;
  }
}

export type AccountSummary = {
  principalId: string; role: string; email: string; emailVerified: boolean; displayName: string; handle: string;
  organizationName: string; organizationSlug: string; accessExpiresAt: string;
};

export function useAccount() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const scope = session.data ? `${session.data.organizationId}:${session.data.principalId}` : "";
  const profile = useQuery({ queryKey: profileKey(scope), enabled: Boolean(scope), queryFn: ({ signal }) => getMyProfile(signal), staleTime: 60_000 });
  const p = profile.data;
  const account: AccountSummary | null = session.data ? {
    principalId: session.data.principalId, role: session.data.role, accessExpiresAt: session.data.accessExpiresAt,
    email: p?.email ?? "", emailVerified: p?.emailVerified ?? false, displayName: p?.displayName ?? "", handle: p?.handle ?? "",
    organizationName: p?.organizationName ?? "", organizationSlug: p?.organizationSlug ?? "",
  } : null;
  return { account, scope, loading: session.isPending || profile.isPending, error: profile.isError };
}

// How a person is shown: display name, then email, then the internal handle.
export const personLabel = (p: { displayName?: string; email?: string; handle?: string; username?: string } | null | undefined) =>
  p?.displayName || p?.email || p?.handle || p?.username || "";

export function initials(account: { displayName?: string; email?: string } | null): string {
  const name = account?.displayName || account?.email?.split("@")[0] || "";
  const parts = name.split(/[\s._+-]+/).filter(Boolean);
  return ((parts[0]?.[0] ?? "?") + (parts.length > 1 ? parts[parts.length - 1][0] : parts[0]?.[1] ?? "")).toUpperCase();
}

// A short, recognizable name for a browser from its User-Agent.
export function browserLabel(userAgent: string): string {
  if (!userAgent) return "Unknown browser";
  const browser = /Edg\//.test(userAgent) ? "Edge" : /Firefox\//.test(userAgent) ? "Firefox" : /Chrome\//.test(userAgent) ? "Chrome"
    : /Safari\//.test(userAgent) ? "Safari" : /curl|Go-http-client|node/i.test(userAgent) ? "Command-line client" : "Browser";
  const system = /iPhone|iPad/.test(userAgent) ? "iOS" : /Android/.test(userAgent) ? "Android" : /Mac OS X|Macintosh/.test(userAgent) ? "macOS"
    : /Windows/.test(userAgent) ? "Windows" : /Linux/.test(userAgent) ? "Linux" : "";
  return system ? `${browser} on ${system}` : browser;
}
