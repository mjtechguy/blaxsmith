import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

globalThis.window = { location: { origin: "https://blaxsmith.test" } };
globalThis.localStorage = { getItem: () => null, setItem: () => {} };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
globalThis.fetch = async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } });

const org = "00000000-0000-4000-8000-000000000001";
const owner = { organizationId: org, principalId: "00000000-0000-4000-8000-000000000002", sessionId: "00000000-0000-4000-8000-000000000003",
  role: "owner", username: "owner", accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() };
const plain = (html) => html.replaceAll("<!-- -->", "");
const route = (id, name, kind) => ({ id, name, kind, connectionId: `c-${id}`, connectionLabel: "", region: kind === "bedrock" ? "us-east-1" : "", cloudProject: "",
  azureResource: "", apiVersion: "", modelMap: {}, weight: 1, priority: 1, concurrencyCap: 0, requestsPerMinute: 0, tokensPerMinute: 0n, state: "enabled",
  poolIds: ["pool-1"], breaker: "closed", cooldownUntil: "", inflight: 0, metrics: [], requests15m: 0, errors15m: 0, last429At: "", stateUpdatedAt: "" });
const pool = { id: "pool-1", name: "Claude production", family: "anthropic", strategy: "priority_headroom", concurrencyCap: 0, affinity: true, state: "enabled",
  routeIds: ["a", "b"], projectIds: ["p1"] };
const routes = { routes: [route("a", "anthropic-a", "anthropic"), route("b", "bedrock-use1", "bedrock")], pools: [pool], paced: [],
  connections: [], projects: [{ id: "p1", label: "Billing", detail: "" }], poolsEnabled: true, pacingEnabled: true };
const detail = {
  pool, hours: 24,
  traffic: [
    { routeId: "a", routeName: "anthropic-a", routeKind: "anthropic", requests: 10n, errors: 2n, rateLimited: 2n, failoversFrom: 2n, tokens: 50000n, costUsdMicros: 1_500_000n, avgTtftMs: 800n },
    { routeId: "b", routeName: "bedrock-use1", routeKind: "bedrock", requests: 2n, errors: 0n, rateLimited: 0n, failoversFrom: 0n, tokens: 9000n, costUsdMicros: 200_000n, avgTtftMs: 0n },
  ],
  failovers: [{ at: new Date().toISOString(), runId: "r1", projectId: "p1", stage: "implement", fromRouteId: "a", fromRouteName: "anthropic-a", httpStatus: 429,
    toRouteId: "b", toRouteName: "bedrock-use1", finalStatus: "ok", finalHttpStatus: 200 }],
};

test("pool detail: routes, traffic and failovers from usage events; Azure routes offered", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp } = await server.ssrLoadModule("/tests/render-app.tsx");
    const { gatewayRoutesKey, poolDetailKey } = await server.ssrLoadModule("/src/gateway.ts");
    const seed = [[gatewayRoutesKey(org), routes], [poolDetailKey(org, "pool-1", 24), detail]];
    const traffic = plain(await renderApp("/admin/pools/pool-1?tab=traffic", owner, seed));
    assert.match(traffic, /Claude production/);
    assert.match(traffic, /Traffic per route/);
    assert.match(traffic, /2 \(20%\)/);
    assert.match(traffic, /\$1\.50/);
    const failovers = plain(await renderApp("/admin/pools/pool-1?tab=failovers", owner, seed));
    assert.match(failovers, /429 rate limited/);
    assert.match(failovers, /bedrock-use1/);
    assert.match(failovers, /Served/);
    const settings = plain(await renderApp("/admin/pools/pool-1?tab=settings", owner, seed));
    assert.match(settings, /Save pool/);
    const list = plain(await renderApp("/admin/routes", owner, [[gatewayRoutesKey(org), routes]]));
    assert.match(list, /Traffic &amp; failovers/);
    assert.match(list, /<option value="azure_openai">Azure OpenAI<\/option>/);
    const member = plain(await renderApp("/admin/pools/pool-1", { ...owner, role: "member" }, seed));
    assert.match(member, /Administration is restricted/);
  } finally {
    await server.close();
  }
});

test("the Codex personal-route switch carries its live-check note", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp } = await server.ssrLoadModule("/tests/render-app.tsx");
    const { gatewaySettingsKey } = await server.ssrLoadModule("/src/gateway.ts");
    const html = plain(await renderApp("/admin/settings/model-gateway", owner, [[gatewaySettingsKey(org), {
      settings: { enabled: true, defaultDeliveryMode: "native_raw", allowProjectChoice: true, removeDirectEgress: true,
        poolsEnabled: false, pacingEnabled: false, personalRoutesEnabled: false, eventRetentionDays: 90 },
      installationAvailable: true, version: 1n, updatedAt: "", updatedByUsername: "" }]]));
    assert.match(html, /id="gw-personal"(?![^>]*checked)/);
    assert.match(html, /not yet been verified against the live ChatGPT backend/);
  } finally {
    await server.close();
  }
});
