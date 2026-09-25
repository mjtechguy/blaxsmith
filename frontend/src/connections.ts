import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { ConnectionService, type Connection, type ConnectionModel } from "./gen/blaxsmith/api/v1/connections_pb";

const client = createClient(ConnectionService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

export type Scope = "organization" | "project" | "personal";
export type ListScope = Scope | "project_available";

export const connectionsKey = (organizationId: string, scope: ListScope, projectId = "") => ["connections", organizationId, scope, projectId] as const;
export const connectionModelsKey = (organizationId: string, connectionId: string, harness: string) => ["connection-models", organizationId, connectionId, harness] as const;
export const gitHubAppKey = (organizationId: string) => ["github-app", organizationId] as const;

export const CLAUDE_SUBSCRIPTION_REASON = "Your organization has not enabled members' own Claude subscriptions. Ask an owner or admin, or add an Anthropic API key.";

export const apiKeyProviders = [
  { id: "anthropic", label: "Anthropic" },
  { id: "openai", label: "OpenAI" },
  { id: "opencode", label: "OpenCode Zen" },
  { id: "opencode-go", label: "OpenCode Go (subscription, API key)" },
] as const;

const providerNames: Record<string, string> = {
  anthropic: "Anthropic", openai: "OpenAI", opencode: "OpenCode Zen", "opencode-go": "OpenCode Go",
  github: "GitHub", gitlab: "GitLab", codex: "Codex (ChatGPT)",
};
export const providerLabel = (provider: string) => providerNames[provider] ?? provider;

// An API-key connection's optional endpoint: what the harness takes as its
// base URL (Anthropic without /v1, OpenAI-compatible with /v1). Not secret.
export const BASE_URL_HELP = "For LiteLLM, a company gateway, or any OpenAI/Anthropic-compatible endpoint";
export const baseUrlPlaceholder = (provider: string) => provider === "anthropic" ? "https://litellm.example.com" : "https://litellm.example.com/v1";

// Mirrors the server's check so mistakes show before the key is sent: https
// (http only for loopback), no credentials, query, or fragment, at most 512
// characters. Returns the normalized URL (trailing slashes stripped), "" for
// empty, or null when invalid.
export function normalizeBaseUrl(raw: string): string | null {
  const value = raw.trim();
  if (!value) return "";
  if (value.length > 512 || /[\s\0]/.test(value)) return null;
  let url: URL;
  try { url = new URL(value); } catch { return null; }
  const loopback = ["localhost", "127.0.0.1", "[::1]"].includes(url.hostname);
  if (!(url.protocol === "https:" || (url.protocol === "http:" && loopback)) || url.username || url.password || url.search || url.hash
    || value.includes("?") || value.includes("#") || !url.hostname) return null;
  return value.replace(/\/+$/, "");
}
export const BASE_URL_INVALID = "Enter an https URL without credentials, a query, or a fragment (at most 512 characters).";

// "OpenAI · Team key", or just the label when it already names the provider
// ("OpenAI sandbox", not "OpenAI · OpenAI sandbox").
export function connectionTitle(c: { provider: string; label: string }): string {
  const provider = providerLabel(c.provider);
  const label = c.label.trim();
  if (!label) return provider;
  return label.toLowerCase().includes(provider.toLowerCase()) ? label : `${provider} · ${label}`;
}
// Who a grant names. The server fills granteeName for users; if it is ever
// missing, show "Unknown user" and a short id rather than a raw principal id.
export function granteeLabel(g: { granteeKind: string; granteeId: string; granteeName: string; projectName?: string }, roleSuffix = "+"): string {
  if (g.granteeKind === "project") return g.projectName || "a project";
  if (g.granteeKind === "role") return `${g.granteeId}${roleSuffix}`;
  return g.granteeName || `Unknown user · ${g.granteeId.slice(0, 8)}`;
}
export const scopeLabel = (scope: string) => scope === "organization" ? "Organization" : scope === "project" ? "Project" : scope === "personal" ? "Personal" : scope;
export const kindLabel = (kind: string) => kind === "api_key" ? "API key" : kind === "git" ? "Git" : kind === "subscription" ? "Subscription" : kind;

// "valid, N models", the provider's error, or unchecked. Git has no models.
export function modelsSummary(c: Pick<Connection, "kind" | "modelCount" | "modelsCheckedAt" | "modelsError">): string {
  if (c.kind === "git") return "—";
  if (c.modelsError) return c.modelsError;
  if (!c.modelsCheckedAt) return "Not checked";
  return `valid, ${c.modelCount} ${c.modelCount === 1 ? "model" : "models"}`;
}

// The one-click fix for a health reason: where to replace a rejected key,
// sign in again, or compare runtime versions. Null when nothing helps.
export function healthFix(c: Pick<Connection, "scope" | "ownerId" | "health">): { to: string; label: string } | null {
  switch (c.health?.reason) {
    case "key_rejected":
      return { label: "Replace key", to: c.scope === "organization" ? "/admin/connections/new/api-key"
        : c.scope === "project" ? `/projects/${c.ownerId}/connections/new/api-key` : "/me/connections/new/api-key" };
    case "needs_sign_in":
    case "token_expiring":
      return { label: "Sign in again", to: "/me/connections/new/subscription" };
    case "harness_behind":
      return { label: "Compare runtimes", to: "/tools" };
  }
  return null;
}

export const authLabel = (auth: string) => auth === "authenticated" ? "Signed in" : auth === "unauthenticated" ? "Not signed in" : "Sign-in not verified";

// Efforts the chosen model accepts (the server already limited them to the
// harness); an unknown model falls back to the harness's own list.
export function effortChoices(model: Pick<ConnectionModel, "efforts"> | undefined, harnessEfforts: readonly string[]): string[] {
  return model?.efforts.length ? [...model.efforts] : [...harnessEfforts];
}

// The effort after choosing a model: its default, else the current effort
// when still allowed, else the first allowed one.
export function effortForModel(model: Pick<ConnectionModel, "efforts" | "defaultEffort"> | undefined, harnessEfforts: readonly string[], current: string): string {
  const choices = effortChoices(model, harnessEfforts);
  if (model?.defaultEffort && choices.includes(model.defaultEffort)) return model.defaultEffort;
  return choices.includes(current) ? current : choices[0] ?? current;
}

export async function listConnections(scope: ListScope, projectId = "", signal?: AbortSignal) {
  return (await client.listConnections({ scope, projectId }, { signal })).connections;
}

// Secrets are write-only: sent once, never returned.
export async function createApiKeyConnection(scope: Scope, projectId: string, provider: string, apiKey: string, label: string, baseUrl = "", signal?: AbortSignal) {
  return (await client.createApiKeyConnection({ scope, projectId, provider, apiKey, label, baseUrl }, { ...(await csrf()), signal })).connection;
}

export async function createGitTokenConnection(scope: Scope, projectId: string, host: string, username: string, token: string) {
  return (await client.createGitTokenConnection({ scope, projectId, host, username, token }, await csrf())).connection;
}

export async function createCodexSubscription(authJson: string) {
  return (await client.createCodexSubscription({ authJson }, await csrf())).connection;
}

export async function createClaudeSubscription(setupToken: string) {
  return (await client.createClaudeSubscription({ setupToken }, await csrf())).connection;
}

export const claudePolicyKey = (org: string) => ["claude-subscription-policy", org] as const;

export async function getClaudeSubscriptionPolicy(signal?: AbortSignal) {
  return (await client.getClaudeSubscriptionPolicy({}, { signal })).allowMemberClaudeSubscription;
}

export async function setClaudeSubscriptionPolicy(allow: boolean) {
  return (await client.setClaudeSubscriptionPolicy({ allowMemberClaudeSubscription: allow }, await csrf())).allowMemberClaudeSubscription;
}

/** The shape `claude setup-token` prints; the server re-checks it. */
export const claudeSetupTokenShape = /^sk-ant-oat[A-Za-z0-9_-]{8,500}$/;

export async function startCodexDeviceLogin() {
  return client.startCodexDeviceLogin({}, await csrf());
}

export async function pollCodexDeviceLogin(loginId: string, signal?: AbortSignal) {
  return client.pollCodexDeviceLogin({ loginId }, { ...(await csrf()), signal });
}

export async function listConnectionModels(connectionId: string, harness = "", signal?: AbortSignal) {
  return client.listConnectionModels({ connectionId, harness }, { signal });
}

export async function refreshConnectionModels(connectionId: string) {
  return client.refreshConnectionModels({ connectionId }, await csrf());
}

export async function grantConnection(connectionId: string, projectId: string, granteeKind: string, granteeId: string) {
  return (await client.grantConnection({ connectionId, projectId, granteeKind, granteeId }, await csrf())).grant;
}

export async function revokeConnectionGrant(grantId: string) {
  return client.revokeConnectionGrant({ grantId }, await csrf());
}

export async function addConnectionUse(connectionId: string, projectId: string, model: string) {
  return (await client.addConnectionUse({ connectionId, projectId, model }, await csrf())).use;
}

export async function removeConnectionUse(useId: string) {
  return client.removeConnectionUse({ useId }, await csrf());
}

export async function setRecommendedModels(connectionId: string, models: string[]) {
  return client.setRecommendedModels({ connectionId, models }, await csrf());
}

export async function setConnectionBaseUrl(connectionId: string, baseUrl: string) {
  return (await client.setConnectionBaseUrl({ connectionId, baseUrl }, await csrf())).connection;
}

export async function revokeConnection(connectionId: string) {
  return client.revokeConnection({ connectionId }, await csrf());
}

export async function listGitRepositories(connectionId: string, query: string, signal?: AbortSignal) {
  return (await client.listGitRepositories({ connectionId, query }, { signal })).repositories;
}

export async function listGitBranches(connectionId: string, fullName: string, signal?: AbortSignal) {
  return (await client.listGitBranches({ connectionId, fullName }, { signal })).branches;
}

export async function getGitHubApp(signal?: AbortSignal) {
  return client.getGitHubApp({}, { signal });
}

export async function setGitHubApp(clientId: string, clientSecret: string) {
  return client.setGitHubApp({ clientId, clientSecret }, await csrf());
}

export async function startGitHubConnect(scope: Scope, projectId: string, returnTo: string) {
  return (await client.startGitHubConnect({ scope, projectId, returnTo }, await csrf())).authorizeUrl;
}
