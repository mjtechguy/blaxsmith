import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { create, fromJson } from "@bufbuild/protobuf";
import type { InfiniteData } from "@tanstack/react-query";
import { browserTransport, csrfToken } from "./auth";
import { EventsAfterResponseSchema, WorkflowEventSchema, WorkflowService, type EventsAfterResponse, type WorkflowEvent } from "./gen/blaxsmith/api/v1/workflow_pb";

const client = createClient(WorkflowService, browserTransport);

export const projectQueries = (organizationId: string, search = "", sortBy = "created_at", sortDirection = "desc") => ["projects", organizationId, search, sortBy, sortDirection] as const;
export const runQueries = (organizationId: string, projectId: string, search = "", sortBy = "created_at", sortDirection = "desc") => ["runs", organizationId, projectId, search, sortBy, sortDirection] as const;

export async function listProjects(pageToken = "", search = "", sortBy = "created_at", sortDirection = "desc", signal?: AbortSignal) {
  return client.listProjects({ pageSize: 20, pageToken, search, sortBy, sortDirection }, { signal });
}

export async function getProject(projectId: string, signal?: AbortSignal) {
  return client.getProject({ projectId }, { signal });
}

export async function createProject(slug: string, name: string) {
  const token = await csrfToken();
  return client.createProject({ slug, name }, { headers: { "X-Blaxsmith-CSRF": token } });
}

export async function listRuns(projectId: string, pageToken = "", search = "", sortBy = "created_at", sortDirection = "desc", signal?: AbortSignal) {
  return client.listRuns({ projectId, pageSize: 20, pageToken, search, sortBy, sortDirection }, { signal });
}

export async function getRun(runId: string, signal?: AbortSignal) {
  return client.getRun({ runId }, { signal });
}

export async function listRunTasks(runId: string, signal?: AbortSignal) {
  return client.listRunTasks({ runId }, { signal });
}

export async function eventsAfter(runId: string, afterId = 0n, signal?: AbortSignal) {
  return client.eventsAfter({ runId, afterId, limit: 100 }, { signal });
}

export async function listCommandExits(runId: string, afterEventId = 0n, signal?: AbortSignal) {
  return client.listCommandExits({ runId, afterEventId, limit: 50 }, { signal });
}

export type RunEventPages = InfiniteData<EventsAfterResponse, bigint>;

export function parseLiveEvent(raw: string, runId: string): WorkflowEvent {
  const event = fromJson(WorkflowEventSchema, JSON.parse(raw));
  if (event.runId !== runId || event.id < 1n) throw new Error("Invalid run event");
  return event;
}

export function appendRunEvent(data: RunEventPages | undefined, event: WorkflowEvent): { data: RunEventPages | undefined; gap: boolean } {
  if (!data?.pages.length) return { data, gap: true };
  const last = data.pages[data.pages.length - 1];
  if (event.id <= last.nextAfterId) return { data, gap: false };
  if (event.id !== last.nextAfterId + 1n) return { data, gap: true };
  if (last.events.length >= 100) return { data: {
    pages: [...data.pages, create(EventsAfterResponseSchema, { events: [event], nextAfterId: event.id })],
    pageParams: [...data.pageParams, last.nextAfterId],
  }, gap: false };
  return { data: { ...data, pages: [...data.pages.slice(0, -1), create(EventsAfterResponseSchema,
    { events: [...last.events, event], nextAfterId: event.id })] }, gap: false };
}

// Catch up in bounded passes so a long-disconnected run does not monopolize the UI.
export async function recoverRunEventBatch(
  fetchPage: (after: bigint) => Promise<EventsAfterResponse>, cursor: () => bigint,
  append: (event: WorkflowEvent) => void,
): Promise<boolean> {
  for (let batch = 0; batch < 5; batch++) {
    const after = cursor();
    const page = await fetchPage(after);
    for (const event of page.events) append(event);
    if (page.events.length < 100 || cursor() <= after) return false;
  }
  return true;
}

export function liveEventsUrl(runId: string, afterId: bigint): string {
  return `${window.location.origin}/api/runs/${encodeURIComponent(runId)}/events?after=${afterId}`;
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
