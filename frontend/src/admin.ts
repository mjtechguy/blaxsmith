import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { AdminService } from "./gen/blaxsmith/api/v1/admin_pb";
import type { SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";

const client = createClient(AdminService, browserTransport);

// The server enforces owner/admin on every AdminService call; this only hides UI.
export const isOrgAdmin = (session?: SessionIdentity | null) => session?.role === "owner" || session?.role === "admin";

export const adminOverviewKey = (organizationId: string) => ["admin-overview", organizationId] as const;
export const auditKey = (organizationId: string, action: string, actor: string, projectId: string) => ["admin-audit", organizationId, action, actor, projectId] as const;

// ponytail: polled every 5s instead of a cross-run stream; the run SSE is per run.
export const ADMIN_REFRESH_MS = 5_000;

export async function getAdminOverview(signal?: AbortSignal) {
  return client.getAdminOverview({}, { signal });
}

export async function listAuditEvents(pageToken: string, action: string, actor: string, projectId: string, signal?: AbortSignal) {
  return client.listAuditEvents({ pageSize: 50, pageToken, action, actor, projectId }, { signal });
}

// Every action this organization has recorded, for the audit log's event filter.
export const auditActionsKey = (organizationId: string) => ["admin-audit-actions", organizationId] as const;
export async function listAuditActions(signal?: AbortSignal) {
  return (await client.listAuditActions({}, { signal })).actions;
}

export async function haltRun(runId: string) {
  const token = await csrfToken();
  return client.haltRun({ runId }, { headers: { "X-Blaxsmith-CSRF": token } });
}

export async function revokeGrant(grantId: string) {
  const token = await csrfToken();
  return client.revokeGrant({ grantId }, { headers: { "X-Blaxsmith-CSRF": token } });
}

export function ago(value: string, now = Date.now()): string {
  if (!value) return "—";
  const seconds = Math.max(0, Math.round((now - Date.parse(value)) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86_400)}d ago`;
}

// The platform's browser session lifetime (serve-app flags; read-only here).
export const sessionPolicyKey = (organizationId: string) => ["admin-session-policy", organizationId] as const;
export async function getSessionPolicy(signal?: AbortSignal) {
  return client.getSessionPolicy({}, { signal });
}

// "7 days", "12 hours", "10 minutes": whole units only, the largest that fits.
export function lifetimeLabel(seconds: bigint | number): string {
  const s = Number(seconds);
  for (const [unit, size] of [["day", 86_400], ["hour", 3_600], ["minute", 60]] as const) {
    if (s >= size && s % size === 0) return `${s / size} ${unit}${s / size === 1 ? "" : "s"}`;
  }
  return `${s} second${s === 1 ? "" : "s"}`;
}
