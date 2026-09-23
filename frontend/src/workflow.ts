import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import { WorkflowService } from "./gen/blaxsmith/api/v1/workflow_pb";

const client = createClient(WorkflowService, browserTransport);

export const projectQueries = (organizationId: string) => ["projects", organizationId] as const;
export const runQueries = (organizationId: string, projectId: string) => ["runs", organizationId, projectId] as const;

export async function listProjects(pageToken = "", signal?: AbortSignal) {
  return client.listProjects({ pageSize: 50, pageToken }, { signal });
}

export async function getProject(projectId: string, signal?: AbortSignal) {
  return client.getProject({ projectId }, { signal });
}

export async function createProject(slug: string, name: string) {
  const token = await csrfToken();
  return client.createProject({ slug, name }, { headers: { "X-Blaxsmith-CSRF": token } });
}

export async function listRuns(projectId: string, pageToken = "", signal?: AbortSignal) {
  return client.listRuns({ projectId, pageSize: 50, pageToken }, { signal });
}

export async function getRun(runId: string, signal?: AbortSignal) {
  return client.getRun({ runId }, { signal });
}

export async function eventsAfter(runId: string, afterId = 0n, signal?: AbortSignal) {
  return client.eventsAfter({ runId, afterId, limit: 100 }, { signal });
}

export async function getCurrentReview(runId: string, signal?: AbortSignal) {
  try {
    return (await client.getCurrentReview({ runId }, { signal })).package ?? null;
  } catch (error) {
    if (ConnectError.from(error).code === Code.NotFound) return null;
    throw error;
  }
}

export async function decideReview(runId: string, packageId: string, action: "approve" | "request_changes", feedback = "") {
  const token = await csrfToken();
  return client.decideReview({ runId, packageId, action, feedback: feedback.trim(), idempotencyKey: crypto.randomUUID() },
    { headers: { "X-Blaxsmith-CSRF": token } });
}
