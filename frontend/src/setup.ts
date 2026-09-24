import { createClient } from "@connectrpc/connect";
import { browserTransport } from "./auth";
import { SetupService, type AccessStep } from "./gen/blaxsmith/api/v1/setup_pb";

const client = createClient(SetupService, browserTransport);

export const explainKey = (org: string, projectId: string, kind: string, resourceId: string, principalId: string) =>
  ["explain-access", org, projectId, kind, resourceId, principalId] as const;

export async function explainAccess(projectId: string, resourceKind: "connection" | "recipe", resourceId: string, principalId = "", signal?: AbortSignal) {
  return client.explainAccess({ projectId, resourceKind, resourceId, principalId }, { signal });
}

export const inspectKey = (org: string, projectId: string) => ["inspect-repository", org, projectId] as const;

export async function inspectRepository(projectId: string, signal?: AbortSignal) {
  return client.inspectRepository({ projectId }, { signal });
}

const stepNames: Record<string, string> = {
  project_grant: "project grant", user_grant: "user grant", role_grant: "role grant",
  organization_connection: "org connection", project_connection: "project connection", personal_connection: "personal connection",
  organization_recipe: "org recipe", project_recipe: "project recipe", recipe_version: "version",
};

// "project grant (Portal) ← org connection 'Anthropic prod' ← version v3 (current)".
export function accessChain(steps: Pick<AccessStep, "kind" | "label">[]): string {
  return steps.map((s) => {
    const name = stepNames[s.kind] ?? s.kind.replaceAll("_", " ");
    if (s.kind.endsWith("_grant")) return s.label ? `${name} (${s.label})` : name;
    if (s.kind === "recipe_version") return `${name} ${s.label}`;
    return s.label ? `${name} '${s.label}'` : name;
  }).join(" ← ");
}
