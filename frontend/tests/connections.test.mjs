import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("connection mutations send CSRF and write-only secrets to ConnectionService", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "C".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    requests.push({ url: String(input), body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") });
    return new Response(JSON.stringify({ connection: { id: "c1", kind: "api_key", provider: "openai", modelCount: 3, modelsCheckedAt: "2026-09-24T00:00:00Z" } }), { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const m = await server.ssrLoadModule("/src/connections.ts");
    const created = await m.createApiKeyConnection("project", "p1", "opencode", "sk-secret", "Zen");
    await m.addConnectionUse("c1", "p1", "opencode/gpt-5.1-codex");
    await m.grantConnection("c1", "", "role", "member");
    await m.listConnections("project_available", "p1");
    assert.deepEqual(requests.map((r) => [r.url.split("/api/")[1], r.csrf]), [
      ["blaxsmith.api.v1.ConnectionService/CreateApiKeyConnection", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/AddConnectionUse", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/GrantConnection", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/ListConnections", null],
    ]);
    assert.deepEqual(requests[0].body, { scope: "project", projectId: "p1", provider: "opencode", apiKey: "sk-secret", label: "Zen" });
    assert.deepEqual(requests[3].body, { scope: "project_available", projectId: "p1" });
    assert.equal(m.modelsSummary(created), "valid, 3 models");
    assert.equal(m.modelsSummary({ kind: "api_key", modelCount: 0, modelsCheckedAt: "x", modelsError: "invalid x-api-key" }), "invalid x-api-key");
    assert.equal(m.modelsSummary({ kind: "git", modelCount: 0, modelsCheckedAt: "", modelsError: "" }), "—");
    assert.match(m.CLAUDE_SUBSCRIPTION_REASON, /Anthropic does not permit/);
    // The Connection message has no secret-bearing fields for the UI to render.
    const { ConnectionSchema } = await server.ssrLoadModule("/src/gen/blaxsmith/api/v1/connections_pb.ts");
    const names = ConnectionSchema.fields.map((f) => f.name);
    assert.ok(!names.some((n) => /key|token|secret|auth_json|password/i.test(n)), names.join(","));
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});
