// Account client seam for the user menu and Account settings.
//
// Today the browser knows the caller's session (role, principal) and, from
// WorkspaceService.GetWorkspaceHome, their organization name, username, and
// display name. Changing the profile, the password, or individual sessions
// needs an AccountService that does not exist yet (GetMyProfile,
// UpdateMyProfile, ChangeMyPassword, ListMySessions, RevokeMySession,
// RevokeMyOtherSessions). When it lands, its generated client goes here and
// `accountActions` flips to true; the pages already have their sections.
import { useQuery } from "@tanstack/react-query";
import { currentSession, sessionQueryKey } from "./auth";
import { getWorkspaceHome, homeKey } from "./workspace";

export const accountActions = false;

export type AccountSummary = {
  principalId: string; role: string; username: string; displayName: string; organizationName: string; organizationSlug: string; accessExpiresAt: string;
};

export function useAccount() {
  const session = useQuery({ queryKey: sessionQueryKey, queryFn: ({ signal }) => currentSession(signal) });
  const scope = session.data ? `${session.data.organizationId}:${session.data.principalId}` : "";
  const home = useQuery({ queryKey: homeKey(scope), enabled: Boolean(scope), queryFn: ({ signal }) => getWorkspaceHome(signal), staleTime: 60_000 });
  const account: AccountSummary | null = session.data ? {
    principalId: session.data.principalId, role: session.data.role, accessExpiresAt: session.data.accessExpiresAt,
    username: home.data?.username ?? "", displayName: home.data?.displayName ?? "",
    organizationName: home.data?.organizationName ?? "", organizationSlug: home.data?.organizationSlug ?? "",
  } : null;
  return { account, loading: session.isPending || home.isPending, error: home.isError };
}

export function initials(account: Pick<AccountSummary, "displayName" | "username"> | null): string {
  const name = account?.displayName || account?.username || "";
  const parts = name.split(/[\s._-]+/).filter(Boolean);
  return ((parts[0]?.[0] ?? "?") + (parts.length > 1 ? parts[parts.length - 1][0] : parts[0]?.[1] ?? "")).toUpperCase();
}
