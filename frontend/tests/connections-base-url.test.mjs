import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync } from "node:fs";
import { createServer as createNet } from "node:net";
import { test } from "node:test";
import { createServer } from "vite";

test("base URLs are normalized in the client and sent with CSRF", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) return new Response(JSON.stringify({ token: "C".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    requests.push({ url: String(input), body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") });
    return new Response(JSON.stringify({ connection: { id: "c1", kind: "api_key", provider: "openai", baseUrl: "https://litellm.example.com/v1" } }), { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const m = await server.ssrLoadModule("/src/connections.ts");
    assert.equal(m.normalizeBaseUrl(""), "");
    assert.equal(m.normalizeBaseUrl("  https://litellm.example.com/v1//  "), "https://litellm.example.com/v1");
    assert.equal(m.normalizeBaseUrl("http://127.0.0.1:4000"), "http://127.0.0.1:4000");
    for (const bad of ["http://litellm.example.com", "https://user:pw@litellm.example.com", "https://litellm.example.com/v1?x=1",
      "https://litellm.example.com/#frag", "ftp://litellm.example.com", "not a url", `https://e.com/${"a".repeat(520)}`]) {
      assert.equal(m.normalizeBaseUrl(bad), null, bad);
    }
    assert.equal(m.baseUrlPlaceholder("anthropic"), "https://litellm.example.com");
    assert.equal(m.baseUrlPlaceholder("openai"), "https://litellm.example.com/v1");
    assert.equal(m.BASE_URL_HELP, "For LiteLLM, a company gateway, or any OpenAI/Anthropic-compatible endpoint");

    await m.createApiKeyConnection("organization", "", "openai", "sk-secret", "LiteLLM", "https://litellm.example.com/v1");
    const updated = await m.setConnectionBaseUrl("c1", "");
    assert.deepEqual(requests.map((r) => [r.url.split("/api/")[1], r.csrf]), [
      ["blaxsmith.api.v1.ConnectionService/CreateApiKeyConnection", "C".repeat(43)],
      ["blaxsmith.api.v1.ConnectionService/SetConnectionBaseUrl", "C".repeat(43)],
    ]);
    assert.deepEqual(requests[0].body, { scope: "organization", provider: "openai", apiKey: "sk-secret", label: "LiteLLM", baseUrl: "https://litellm.example.com/v1" });
    assert.deepEqual(requests[1].body, { connectionId: "c1" }); // Empty clears; proto JSON omits it.
    assert.equal(updated.baseUrl, "https://litellm.example.com/v1");

    // No model-gateway service is left in the generated API.
    const { ConnectionService } = await server.ssrLoadModule("/src/gen/blaxsmith/api/v1/connections_pb.ts");
    assert.ok(ConnectionService.method.setConnectionBaseUrl);
    assert.ok(!existsSync(new URL("../src/gen/blaxsmith/api/v1/gateway_pb.ts", import.meta.url)));
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});

const freePort = () => new Promise((resolve, reject) => {
  const probe = createNet().once("error", reject);
  probe.listen(0, "127.0.0.1", () => { const { port } = probe.address(); probe.close(() => resolve(port)); });
});

test("the mock server validates base URLs, falls back when an endpoint lists no models, and has no usage or cost RPCs", async () => {
  const [apiPort, webPort] = [await freePort(), await freePort()];
  const child = spawn(process.execPath, ["dev/mock-server.mjs"], { env: { ...process.env, MOCK_API_PORT: String(apiPort), MOCK_WEB_PORT: String(webPort) }, stdio: ["ignore", "pipe", "pipe"] });
  try {
    await new Promise((resolve, reject) => {
      let out = "";
      const timer = setTimeout(() => reject(new Error(`mock did not start: ${out}`)), 30_000);
      child.stdout.on("data", (chunk) => { out += chunk; if (out.includes("[mock] API on")) { clearTimeout(timer); resolve(); } });
      child.once("exit", (code) => { clearTimeout(timer); reject(new Error(`mock exited ${code}: ${out}`)); });
    });
    const call = async (service, method, body) => {
      const res = await fetch(`http://127.0.0.1:${apiPort}/api/blaxsmith.api.v1.${service}/${method}`, { method: "POST", headers: { "content-type": "application/json" }, body: JSON.stringify(body) });
      return { status: res.status, body: await res.json() };
    };
    const create = (baseUrl) => call("ConnectionService", "CreateApiKeyConnection", { scope: "organization", provider: "openai", apiKey: "sk-x", label: "LiteLLM", baseUrl });

    const ok = await create("https://litellm.example.com/v1/");
    assert.equal(ok.status, 200);
    assert.equal(ok.body.connection.baseUrl, "https://litellm.example.com/v1");
    assert.equal(ok.body.connection.modelsError, "");
    for (const bad of ["http://litellm.example.com", "https://u:p@litellm.example.com", "https://litellm.example.com/?q=1", "https://litellm.example.com/#x"]) {
      const res = await create(bad);
      assert.equal(res.status, 400, bad);
      assert.equal(res.body.code, "invalid_argument");
    }

    const id = ok.body.connection.id;
    const cleared = await call("ConnectionService", "SetConnectionBaseUrl", { connectionId: id, baseUrl: "" });
    assert.equal(cleared.status, 200);
    assert.equal(cleared.body.connection.baseUrl, "");
    const none = await call("ConnectionService", "SetConnectionBaseUrl", { connectionId: id, baseUrl: "https://nomodels.example.com/v1" });
    assert.equal(none.body.connection.modelsError, "endpoint did not list models");
    const models = await call("ConnectionService", "ListConnectionModels", { connectionId: id });
    assert.deepEqual([models.body.models, models.body.error], [[], "endpoint did not list models"]);
    assert.equal((await call("ConnectionService", "SetConnectionBaseUrl", { connectionId: id, baseUrl: "https://x.example.com/v1?key=1" })).status, 400);

    // Subscriptions never carry a base URL.
    const sub = await call("ConnectionService", "CreateCodexSubscription", { authJson: "{}" });
    assert.equal((await call("ConnectionService", "SetConnectionBaseUrl", { connectionId: sub.body.connection.id, baseUrl: "https://litellm.example.com/v1" })).status, 400);

    for (const [service, method] of [["UsageService", "GetRunCost"], ["UsageService", "GetMyUsage"], ["UsageService", "GetProjectUsage"],
      ["GatewayAdminService", "GetUsageOverview"], ["GatewayAdminService", "ListBudgets"], ["GatewayAdminService", "GetGatewaySettings"]]) {
      const res = await call(service, method, {});
      assert.equal(res.status, 404, method);
      assert.equal(res.body.code, "unimplemented", method);
    }
    const inbox = await call("WorkspaceService", "ListInbox", {});
    assert.ok(!JSON.stringify(inbox.body).includes("budget_alert"));
  } finally {
    child.kill();
  }
});
