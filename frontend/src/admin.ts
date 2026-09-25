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

export async function haltRun(runId: string) {
  const token = await csrfToken();
  return client.haltRun({ runId }, { headers: { "X-Blaxsmith-CSRF": token } });
}

export async function revokeGrant(grantId: string) {
  const token = await csrfToken();
  return client.revokeGrant({ grantId }, { headers: { "X-Blaxsmith-CSRF": token } });
}

export const auditActions = [
  "access.git_connection.created", "access.grant.revoked", "access.project_model.created", "access.project_model.revoked",
  "access.resource_grant.created", "access.resource_grant.revoked", "gateway.price.overridden", "gateway.project_delivery.updated", "gateway.settings.updated",
  "gateway.pool.created", "gateway.pool.updated", "gateway.route.created", "gateway.route.disabled", "gateway.route.draining", "gateway.route.enabled", "gateway.route.updated",
  "identity.account.reset_completed", "identity.account.setup_completed",
  "identity.login", "identity.logout", "identity.refresh", "identity.refresh_reuse", "identity.user.disabled", "identity.user.enabled",
  "identity.user.invited", "identity.user.reset_link_issued", "identity.user.role_changed", "identity.user.sessions_revoked",
  "identity.user.setup_link_issued", "installation.bootstrap_owner",
  "workflow.attempt.steered", "workflow.extension.installed", "workflow.interaction.answered", "workflow.project.created", "workflow.project_source.set",
  "workflow.project_verification.set", "workflow.recipe.created", "workflow.recipe.current_set", "workflow.recipe.version_created",
  "workflow.review.presented", "workflow.review.superseded",
  "workflow.run.halted", "workflow.run.launched",
];

export function ago(value: string, now = Date.now()): string {
  if (!value) return "—";
  const seconds = Math.max(0, Math.round((now - Date.parse(value)) / 1000));
  if (seconds < 60) return `${seconds}s ago`;
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)}h ago`;
  return `${Math.floor(seconds / 86_400)}d ago`;
}
