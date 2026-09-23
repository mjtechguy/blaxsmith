import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient } from "@tanstack/react-query";
import { createServer } from "vite";

test("review correction sends trimmed feedback with CSRF", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  let request;
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "A".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    request = { url: String(input), body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") };
    return new Response(JSON.stringify({ decision: { id: "decision" } }), { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const { decideReview } = await server.ssrLoadModule("/src/workflow.ts");
    await decideReview("run", "package", "request_changes", "  Fix the failing check.  ");
    assert.equal(request.url, "https://blaxsmith.test/api/blaxsmith.api.v1.WorkflowService/DecideReview");
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.feedback, "Fix the failing check.");
    assert.equal(request.body.action, "request_changes");
    assert.equal(request.body.packageId, "package");
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});

test("live activity merges replay once and detects missing events", async () => {
  const previousWindow = globalThis.window;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { appendRunEvent, liveEventsUrl, parseLiveEvent, recoverRunEventBatch } = await server.ssrLoadModule("/src/workflow.ts");
    const parse = (id, runId = "run") => parseLiveEvent(JSON.stringify({ id: String(id), runId, kind: "attempt.started", occurredAt: "2026-09-23T00:00:00Z" }), "run");
    assert.equal(liveEventsUrl("run", 12n), "https://blaxsmith.test/api/runs/run/events?after=12");
    assert.throws(() => parse(1, "other"), /Invalid run event/);
    let data = { pages: [{ events: [], nextAfterId: 0n }], pageParams: [0n] };
    for (let id = 1; id <= 101; id++) {
      const merged = appendRunEvent(data, parse(id));
      assert.equal(merged.gap, false);
      data = merged.data;
    }
    assert.equal(data.pages.length, 2);
    assert.equal(data.pages[1].nextAfterId, 101n);
    assert.deepEqual(data.pageParams, [0n, 100n]);
    assert.equal(appendRunEvent(data, parse(101)).data, data);
    assert.equal(appendRunEvent(data, parse(103)).gap, true);
    assert.equal(data.pages[1].events.length, 1);
    data = { pages: [{ events: [], nextAfterId: 0n }], pageParams: [0n] };
    const cursor = () => data.pages.at(-1).nextAfterId;
    const fetchPage = async (after) => {
      const end = Math.min(Number(after) + 100, 600);
      return { events: Array.from({ length: end - Number(after) }, (_, index) => parse(Number(after) + index + 1)), nextAfterId: BigInt(end) };
    };
    const append = (event) => { data = appendRunEvent(data, event).data; };
    assert.equal(await recoverRunEventBatch(fetchPage, cursor, append), true);
    assert.equal(cursor(), 500n);
    assert.equal(await recoverRunEventBatch(fetchPage, cursor, append), false);
    assert.equal(cursor(), 600n);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
  }
});

test("session scope change removes cached run activity", async () => {
  const previousWindow = globalThis.window;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { clearWorkspaceCache, sessionQueryKey } = await server.ssrLoadModule("/src/auth.ts");
    const queryClient = new QueryClient();
    queryClient.setQueryData(sessionQueryKey, { organizationId: "first", principalId: "user" });
    queryClient.setQueryData(["run-events", "first:user", "run"], { pages: [{ events: [{ id: 1n }], nextAfterId: 1n }], pageParams: [0n] });
    await clearWorkspaceCache(queryClient);
    assert.equal(queryClient.getQueryData(["run-events", "first:user", "run"]), undefined);
    assert.equal(queryClient.getQueryData(sessionQueryKey).organizationId, "first");
  } finally {
    await server.close();
    globalThis.window = previousWindow;
  }
});
