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
    await m.createClaudeSubscription("sk-ant-oat01-write-only");
    await m.setClaudeSubscriptionPolicy(true);
    await m.getClaudeSubscriptionPolicy();
    assert.deepEqual(requests.map((r) => [r.url.split("/api/")[1], r.csrf]), [
      ["blaxsmith.api.v1.ConnectionService/CreateApiKeyConnection", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/AddConnectionUse", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/GrantConnection", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/ListConnections", null],
      ["blaxsmith.api.v1.ConnectionService/CreateClaudeSubscription", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/SetClaudeSubscriptionPolicy", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/GetClaudeSubscriptionPolicy", null],
    ]);
    assert.deepEqual(requests[4].body, { setupToken: "sk-ant-oat01-write-only" });
    assert.deepEqual(requests[0].body, { scope: "project", projectId: "p1", provider: "opencode", apiKey: "sk-secret", label: "Zen" });
    assert.deepEqual(requests[3].body, { scope: "project_available", projectId: "p1" });
    assert.equal(m.modelsSummary(created), "valid, 3 models");
    assert.equal(m.modelsSummary({ kind: "api_key", modelCount: 0, modelsCheckedAt: "x", modelsError: "invalid x-api-key" }), "invalid x-api-key");
    assert.equal(m.modelsSummary({ kind: "git", modelCount: 0, modelsCheckedAt: "", modelsError: "" }), "—");
    assert.match(m.CLAUDE_SUBSCRIPTION_REASON, /has not enabled members. own Claude subscriptions/);
    assert.ok(m.claudeSetupTokenShape.test("sk-ant-oat01-" + "a".repeat(40)) && !m.claudeSetupTokenShape.test("sk-ant-api03-" + "a".repeat(40)));
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

test("effort choices follow the chosen model and preselect its default", async () => {
  const previousWindow = globalThis.window;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const m = await server.ssrLoadModule("/src/connections.ts");
    const harness = ["low", "medium", "high", "xhigh", "max"];
    const opus46 = { efforts: ["low", "medium", "high", "max"], defaultEffort: "high" };
    assert.deepEqual(m.effortChoices(opus46, harness), ["low", "medium", "high", "max"]);
    assert.deepEqual(m.effortChoices({ efforts: [] }, harness), harness);
    assert.deepEqual(m.effortChoices(undefined, harness), harness);
    assert.equal(m.effortForModel(opus46, harness, "xhigh"), "high");
    assert.equal(m.effortForModel({ efforts: ["low", "max"], defaultEffort: "" }, harness, "max"), "max");
    assert.equal(m.effortForModel({ efforts: ["low", "max"], defaultEffort: "" }, harness, "xhigh"), "low");
    assert.equal(m.effortForModel(undefined, harness, "high"), "high");
    const health = (reason) => ({ reason });
    assert.deepEqual(m.healthFix({ scope: "project", ownerId: "p1", health: health("key_rejected") }), { label: "Replace key", to: "/projects/p1/connections/new/api-key" });
    assert.equal(m.healthFix({ scope: "organization", ownerId: "o", health: health("key_rejected") }).to, "/admin/connections/new/api-key");
    assert.equal(m.healthFix({ scope: "personal", ownerId: "u", health: health("needs_sign_in") }).to, "/me/connections/new/subscription");
    assert.equal(m.healthFix({ scope: "personal", ownerId: "u", health: health("") }), null);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
  }
});
