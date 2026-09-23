import assert from "node:assert/strict";
import { test } from "node:test";
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
