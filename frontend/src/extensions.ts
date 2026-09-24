import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { ExtensionService, type ExtensionPermission, type ExtensionTemplate } from "./gen/blaxsmith/api/v1/extensions_pb";

const client = createClient(ExtensionService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

export const extensionsKey = (org: string, projectId = "") => ["extensions", org, projectId] as const;
export const extensionKey = (org: string, extensionId: string) => ["extension", org, extensionId] as const;

export type ExtensionSourceInput = { repositoryUrl: string; gitRef: string; manifestPath: string; overlayManifestJson: string };

// With a project: only the extensions the caller may use there (a grant).
export const listExtensions = (projectId = "", signal?: AbortSignal) => client.listExtensions({ projectId }, { signal });
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

// Stage templates a recipe stage of this kind can name, from every installed version.
export type TemplateChoice = { reference: string; title: string; harness: string; extension: string; current: boolean };
export function templateChoices(versions: Array<{ version: string; templates: ExtensionTemplate[] }>, currentVersion: string, kind: string, extensionKey: string): TemplateChoice[] {
  return versions.flatMap((v) => v.templates.filter((t) => !kind || t.kinds.includes(kind)).map((t) => ({
    reference: t.reference, title: t.title || t.id, harness: t.harness, extension: extensionKey, current: v.version === currentVersion })));
}

export const permissionKinds: Record<string, string> = {
  subagents: "Native subagents", mcp: "MCP server", hook: "Hook", egress: "Network egress", runtime: "Runner runtime",
};
