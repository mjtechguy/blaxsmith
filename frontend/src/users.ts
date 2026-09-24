import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import type { SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";
import { UserAdminService } from "./gen/blaxsmith/api/v1/users_pb";

const client = createClient(UserAdminService, browserTransport);

export const membersKey = (organizationId: string) => ["org-members", organizationId] as const;
export const memberRoles = ["owner", "admin", "member", "viewer"] as const;
export type MemberRole = (typeof memberRoles)[number];

// UI-only mirror of the server rule: only owners grant owner or change an owner.
export const assignableRoles = (session?: SessionIdentity | null): MemberRole[] =>
  session?.role === "owner" ? [...memberRoles] : session?.role === "admin" ? ["admin", "member", "viewer"] : [];
export const canManage = (session: SessionIdentity | null | undefined, targetRole: string) =>
  session?.role === "owner" || (session?.role === "admin" && targetRole !== "owner");

export const setupPath = (token: string) => `/setup/${encodeURIComponent(token)}`;
export const setupUrl = (token: string, origin = window.location.origin) => `${origin}${setupPath(token)}`;

async function mutation() {
  return { headers: { "X-Blaxsmith-CSRF": await csrfToken() } };
}

export async function listMembers(signal?: AbortSignal) {
  return client.listOrgMembers({}, { signal });
}

export async function inviteUser(username: string, displayName: string, role: string) {
  return client.inviteUser({ username, displayName, role }, await mutation());
}

export async function setUserRole(principalId: string, role: string) {
  return client.setUserRole({ principalId, role }, await mutation());
}

export async function setUserEnabled(principalId: string, enabled: boolean) {
  return client.setUserEnabled({ principalId, enabled }, await mutation());
}

export async function issueResetLink(principalId: string) {
  return client.issueResetLink({ principalId }, await mutation());
}

export async function revokeUserSessions(principalId: string) {
  return client.revokeUserSessions({ principalId }, await mutation());
}

export async function getAccountLink(token: string, signal?: AbortSignal) {
  return client.getAccountLink({ token }, { signal });
}

export async function completeAccountLink(token: string, password: string) {
  return client.completeAccountLink({ token, password }, await mutation());
}

export function userAdminError(cause: unknown): string {
  const error = ConnectError.from(cause);
  switch (error.code) {
    case Code.PermissionDenied: return error.rawMessage.includes("owner") ? "Only an owner can grant owner access or change an owner."
      : error.rawMessage.includes("another organization") ? "This account also belongs to another organization, so its password cannot be reset here."
        : "Your session is not allowed to do this.";
    case Code.FailedPrecondition: return error.rawMessage.includes("yourself") || error.rawMessage.includes("your own")
      ? "You cannot disable your own account." : "The organization must keep at least one active owner who can sign in.";
    case Code.AlreadyExists: return "That username is already taken.";
    case Code.NotFound: return "This member no longer exists. The list will refresh.";
    case Code.InvalidArgument: return "Check the details and try again.";
    case Code.Unauthenticated: return "Your session changed. Sign in again and retry.";
    default: return "The change could not be completed. Please try again.";
  }
}
