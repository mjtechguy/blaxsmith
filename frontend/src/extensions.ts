import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { ExtensionService, type ExtensionPermission } from "./gen/blaxsmith/api/v1/extensions_pb";

const client = createClient(ExtensionService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

export const extensionsKey = (org: string) => ["extensions", org] as const;
export const extensionKey = (org: string, extensionId: string) => ["extension", org, extensionId] as const;

export type ExtensionSourceInput = { repositoryUrl: string; gitRef: string; manifestPath: string; overlayManifestJson: string };

export const listExtensions = (signal?: AbortSignal) => client.listExtensions({}, { signal });
export const getExtension = (extensionId: string, signal?: AbortSignal) => client.getExtension({ extensionId }, { signal });

// Preview fetches the ref and validates; it saves nothing but is admin-only, so it carries CSRF too.
export async function previewExtensionInstall(source: ExtensionSourceInput) {
  return client.previewExtensionInstall({ source }, await csrf());
}
export async function installExtension(source: ExtensionSourceInput, expectedCommit: string, approvedPermissions: string[]) {
  return client.installExtension({ source, expectedCommit, approvedPermissions }, await csrf());
}
export async function checkExtensionUpdate(extensionId: string) {
  return (await client.checkExtensionUpdate({ extensionId }, await csrf())).extension;
}
export async function grantExtension(extensionId: string, projectId: string, granteeKind: string, granteeId: string) {
  return (await client.grantExtension({ extensionId, projectId, granteeKind, granteeId }, await csrf())).grant;
}
export async function revokeExtensionGrant(grantId: string) {
  return client.revokeExtensionGrant({ grantId }, await csrf());
}

// Required permissions are always approved; optional ones only when chosen.
export function approvedPermissions(permissions: Pick<ExtensionPermission, "id" | "optional">[], chosen: ReadonlySet<string>): string[] {
  return permissions.filter((p) => !p.optional || chosen.has(p.id)).map((p) => p.id).sort();
}

export const permissionKinds: Record<string, string> = {
  subagents: "Native subagents", mcp: "MCP server", hook: "Hook", egress: "Network egress", runtime: "Runner runtime",
};
