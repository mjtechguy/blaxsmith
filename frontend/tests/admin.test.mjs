import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("admin actions send CSRF to AdminService and nav gating follows role", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "B".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    requests.push({ url: String(input), body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") });
    return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const { haltRun, revokeGrant, isOrgAdmin, ago } = await server.ssrLoadModule("/src/admin.ts");
    await haltRun("run-1");
    await revokeGrant("grant-1");
    assert.deepEqual(requests.map((r) => [r.url.split("/api/")[1], r.csrf, r.body]), [
      ["blaxsmith.api.v1.AdminService/HaltRun", "B".repeat(43), { runId: "run-1" }],
      ["blaxsmith.api.v1.AdminService/RevokeGrant", "B".repeat(43), { grantId: "grant-1" }],
    ]);
    assert.deepEqual(["owner", "admin", "member", "viewer"].map((role) => isOrgAdmin({ role })), [true, true, false, false]);
    assert.equal(isOrgAdmin(undefined), false);
    assert.equal(ago(new Date(1_000_000 - 125_000).toISOString(), 1_000_000), "2m ago");
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});
