import { createClient } from "@connectrpc/connect";
import { browserTransport } from "./auth";
import { WorkspaceService } from "./gen/blaxsmith/api/v1/workspace_pb";
import type { TableView } from "./table-state";

const client = createClient(WorkspaceService, browserTransport);

// Keys carry the organization and principal: results depend on the caller's role.
export const homeKey = (scope: string) => ["workspace-home", scope] as const;
export const inboxKey = (scope: string, view: TableView, actionable: boolean, projectId = "") => ["workspace-inbox", scope, projectId, actionable, view.q, view.page, view.size, view.filters] as const;
export const workspaceRunsKey = (scope: string, view: TableView, projectId = "") => ["workspace-runs", scope, projectId, view.q, view.sort, view.page, view.size, view.filters] as const;
export const membersPageKey = (scope: string, view: TableView) => ["workspace-members", scope, view.q, view.sort, view.page, view.size, view.filters] as const;

export const HOME_REFRESH_MS = 15_000;

export const getWorkspaceHome = (signal?: AbortSignal) => client.getWorkspaceHome({}, { signal });

export const listInbox = (view: TableView, actionableOnly: boolean, projectId = "", signal?: AbortSignal) =>
  client.listInbox({ page: view.page, pageSize: view.size, search: view.q, kinds: view.filters.kind ?? [], projectId, actionableOnly }, { signal });

const runSort: Record<string, string> = { run: "launch_key", project: "project", state: "state", created: "created_at" };
export const listWorkspaceRuns = (view: TableView, projectId = "", signal?: AbortSignal) =>
  client.listWorkspaceRuns({ page: view.page, pageSize: view.size, search: view.q, states: view.filters.state ?? [], projectId,
    sortBy: runSort[view.sort[0]?.id ?? "created"] ?? "created_at", sortDirection: view.sort[0]?.desc === false ? "asc" : "desc" }, { signal });

const memberSort: Record<string, string> = { member: "email", role: "role", lastLogin: "last_login", created: "created_at" };
export const listMembersPage = (view: TableView, signal?: AbortSignal) =>
  client.listMembersPage({ page: view.page, pageSize: view.size, search: view.q, roles: view.filters.role ?? [], statuses: view.filters.status ?? [],
    sortBy: memberSort[view.sort[0]?.id ?? "role"] ?? "role", sortDirection: view.sort[0]?.desc ? "desc" : "asc" }, { signal });

export const runStates = [
  { value: "queued", label: "Queued" }, { value: "active", label: "Active" }, { value: "cancel_requested", label: "Cancelling" },
  { value: "cancelled", label: "Cancelled" }, { value: "failed", label: "Failed" }, { value: "succeeded", label: "Succeeded" },
];
export const inboxKinds = [
  { value: "approval", label: "Approval" }, { value: "question", label: "Question" }, { value: "escalation", label: "Escalation" },
  { value: "interview_round", label: "Interview" }, { value: "review", label: "Final review" }, { value: "budget_alert", label: "Budget alert" },
];
export const kindLabel = (kind: string) => inboxKinds.find((k) => k.value === kind)?.label ?? kind.replaceAll("_", " ");
