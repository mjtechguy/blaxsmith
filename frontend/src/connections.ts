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

export const CLAUDE_SUBSCRIPTION_REASON = "Anthropic does not permit third-party platforms to collect, store, or intermediate Claude.ai credentials; use an Anthropic API key.";

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
export const scopeLabel = (scope: string) => scope === "organization" ? "Organization" : scope === "project" ? "Project" : scope === "personal" ? "Personal" : scope;
export const kindLabel = (kind: string) => kind === "api_key" ? "API key" : kind === "git" ? "Git" : kind === "subscription" ? "Subscription" : kind;

// "valid, N models", the provider's error, or unchecked. Git has no models.
export function modelsSummary(c: Pick<Connection, "kind" | "modelCount" | "modelsCheckedAt" | "modelsError">): string {
  if (c.kind === "git") return "—";
  if (c.modelsError) return c.modelsError;
  if (!c.modelsCheckedAt) return "Not checked";
  return `valid, ${c.modelCount} ${c.modelCount === 1 ? "model" : "models"}`;
}

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
export async function createApiKeyConnection(scope: Scope, projectId: string, provider: string, apiKey: string, label: string) {
  return (await client.createApiKeyConnection({ scope, projectId, provider, apiKey, label }, await csrf())).connection;
}

export async function createGitTokenConnection(scope: Scope, projectId: string, host: string, username: string, token: string) {
  return (await client.createGitTokenConnection({ scope, projectId, host, username, token }, await csrf())).connection;
}

export async function createCodexSubscription(authJson: string) {
  return (await client.createCodexSubscription({ authJson }, await csrf())).connection;
}

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
