import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

// The app modules read these browser globals at import time.
globalThis.window = { location: { origin: "https://blaxsmith.test" } };
globalThis.localStorage = { getItem: () => null, setItem: () => {} };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
globalThis.fetch = async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } });

const org = "00000000-0000-4000-8000-000000000001";
const owner = { organizationId: org, principalId: "00000000-0000-4000-8000-000000000002", sessionId: "00000000-0000-4000-8000-000000000003",
  role: "owner", username: "owner", accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() };
const soon = (seconds) => new Date(Date.now() + seconds * 1000).toISOString();
// React separates adjacent text nodes with comments in server output.
const plain = (html) => html.replaceAll("<!-- -->", "");

const route = (id, name, extra = {}) => ({ id, name, kind: "anthropic", connectionId: `c-${id}`, connectionLabel: `key ${id}`, region: "", cloudProject: "",
  modelMap: {}, weight: 1, priority: 1, concurrencyCap: 0, requestsPerMinute: 0, tokensPerMinute: 0n, state: "enabled", poolIds: ["pool-1"],
  breaker: "closed", cooldownUntil: "", inflight: 0, metrics: [], requests15m: 0, errors15m: 0, last429At: "", stateUpdatedAt: "", ...extra });

const routesData = (overrides = {}) => ({
  routes: [
    route("a", "anthropic-a", { breaker: "open", cooldownUntil: soon(30), errors15m: 5, requests15m: 9, inflight: 1, concurrencyCap: 4,
      metrics: [{ name: "requests", limit: 50n, remaining: 0n, resetAt: soon(42) }, { name: "tokens", limit: 400000n, remaining: 1200n, resetAt: soon(42) }] }),
    route("b", "bedrock-use1", { kind: "bedrock", region: "us-east-1", priority: 2, modelMap: { "claude-opus-5-5": "us.anthropic.claude-opus-5-5-v1:0" }, state: "draining" }),
  ],
  pools: [{ id: "pool-1", name: "Claude production", family: "anthropic", strategy: "priority_headroom", concurrencyCap: 0, affinity: true, state: "enabled",
    routeIds: ["a", "b"], projectIds: ["p1"] }],
  paced: [{ taskId: "t1", runId: "r1", projectId: "p1", stage: "implement", poolId: "pool-1", reason: "Waiting for Claude production headroom",
    resetsAt: soon(42), since: new Date().toISOString() }],
  connections: [{ id: "c-a", label: "Anthropic key A", detail: "anthropic" }],
  projects: [{ id: "p1", label: "Billing", detail: "" }],
  poolsEnabled: true, pacingEnabled: false, ...overrides,
});

test("countdowns, window labels and model mappings", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { countdown, windowLabel } = await server.ssrLoadModule("/src/gateway.ts");
    const now = Date.parse("2026-09-24T12:00:00Z");
    assert.equal(countdown("2026-09-24T12:00:42Z", now), "42 s");
    assert.equal(countdown("2026-09-24T12:30:00Z", now), "30 min");
    assert.equal(countdown("2026-09-24T15:10:00Z", now), "3 h 10 min");
    assert.equal(countdown("2026-09-24T11:59:00Z", now), ""); // past resets are not shown as negative
    assert.equal(countdown("", now), "");
    assert.equal(windowLabel("primary", 300), "Primary (5-hour window)");
    assert.equal(windowLabel("secondary", 10080), "Secondary (7-day window)");
    assert.equal(windowLabel("claude_5h", 300), "5h (5-hour window)");
    const { parseModelMap } = await server.ssrLoadModule("/src/routes/admin.routes.tsx");
    assert.deepEqual(parseModelMap("claude-opus-5-5 = us.anthropic.claude-opus-5-5-v1:0\n\n"), { "claude-opus-5-5": "us.anthropic.claude-opus-5-5-v1:0" });
    assert.deepEqual(parseModelMap(""), {});
    assert.equal(parseModelMap("no-equals-sign"), null);
    assert.equal(parseModelMap("a=b=c"), null);
  } finally {
    await server.close();
  }
});

test("Routes & pools: health, headroom with reset countdowns, paced stages, pools", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp } = await server.ssrLoadModule("/tests/render-app.tsx");
    const { gatewayRoutesKey } = await server.ssrLoadModule("/src/gateway.ts");
    const html = plain(await renderApp("/admin/routes", owner, [[gatewayRoutesKey(org), routesData()]]));
    assert.match(html, /Routes &amp; pools/);
    assert.match(html, /anthropic-a/);
    assert.match(html, /Breaker open/);
    assert.match(html, /cooling down, (29|30) s/);
    assert.match(html, /aria-label="0 of 50 remaining"/);
    assert.match(html, /resets in (41|42) s/);
    assert.match(html, /Amazon Bedrock · us-east-1/);
    assert.match(html, /Draining/);
    assert.match(html, /Queued for headroom/);
    assert.match(html, /Waiting for Claude production headroom/);
    assert.match(html, /Rate-aware pacing is off/);
    assert.match(html, /Claude production/);
    assert.match(html, /Add a route/);
    // Credentials are write-only; nothing secret-shaped is rendered.
    assert.doesNotMatch(html, /secret_access_key"\s*:\s*"[^…]/);
    assert.doesNotMatch(html, /style=/);
    // A member never sees the page.
    const member = await renderApp("/admin/routes", { ...owner, role: "member" }, [[gatewayRoutesKey(org), routesData()]]);
    assert.match(member, /Administration is restricted/);
    assert.doesNotMatch(member, /anthropic-a/);
  } finally {
    await server.close();
  }
});

test("settings show the G2/G4 switches; My usage shows only my own subscription meters", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp } = await server.ssrLoadModule("/tests/render-app.tsx");
    const { gatewaySettingsKey, myUsageKey, mySubscriptionLimitsKey } = await server.ssrLoadModule("/src/gateway.ts");
    const settings = plain(await renderApp("/admin/settings/model-gateway", owner, [[gatewaySettingsKey(org), {
      settings: { enabled: true, defaultDeliveryMode: "native_raw", allowProjectChoice: true, removeDirectEgress: true,
        poolsEnabled: true, pacingEnabled: false, personalRoutesEnabled: false, eventRetentionDays: 90 },
      installationAvailable: true, version: 3n, updatedAt: "", updatedByUsername: "" }]]));
    assert.match(settings, /id="gw-pools"[^>]*checked/);
    assert.match(settings, /Rate-aware pacing/);
    assert.match(settings, /Personal subscription routes/);
    assert.match(settings, /never pooled, shared or rotated/);
    assert.match(settings, /id="gw-retention"[^>]*value="90"/);
    assert.doesNotMatch(settings, /Coming in G2/); // shipped switches are real now
    assert.match(settings, /Coming in a later phase/); // content capture is still a later phase

    const scope = `${org}:${owner.principalId}`;
    const totals = { requests: 0n, errors: 0n, rateLimited: 0n, inputTokens: 0n, outputTokens: 0n, cacheReadTokens: 0n, cacheWriteTokens: 0n, reasoningTokens: 0n, costUsdMicros: 0n };
    const usage = plain(await renderApp("/me/usage", owner, [
      [myUsageKey(scope, 30), { enabled: true, fromDay: "2026-09-01", toDay: "2026-09-30", totals, byProject: [], byModel: [], topRuns: [] }],
      [mySubscriptionLimitsKey(scope), { personalRoutesEnabled: true, subscriptions: [
        { connectionId: "c1", label: "My Codex", provider: "openai", authMethod: "codex_chatgpt", state: "active",
          windows: [{ name: "primary", usedPct: 64, windowMinutes: 300, resetsAt: soon(900), observedAt: new Date().toISOString() }] },
        { connectionId: "c2", label: "", provider: "anthropic", authMethod: "claude_setup_token", state: "active", windows: [] },
      ] }],
    ]));
    assert.match(usage, /My subscriptions/);
    assert.match(usage, /My Codex/);
    assert.match(usage, /Primary \(5-hour window\)/);
    assert.match(usage, /64% used · resets in 15 min/);
    assert.match(usage, /<meter[^>]*value="64"/);
    assert.match(usage, /Claude subscription/);
    assert.match(usage, /Not reported by provider yet/);
  } finally {
    await server.close();
  }
});

test("a paced stage shows its reason and reset countdown", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const React = await import("react");
    const { renderInRouter } = await server.ssrLoadModule("/tests/render-app.tsx");
    const { PacedChip } = await server.ssrLoadModule("/src/paced-chip.tsx");
    const html = plain(await renderInRouter(React.createElement(PacedChip, { reason: "Waiting for Claude production headroom", resetsAt: soon(42) })));
    assert.match(html, /Waiting for Claude production headroom, resets in (41|42) s/);
    assert.equal(await renderInRouter(React.createElement(PacedChip, { reason: "", resetsAt: "" })), "");
  } finally {
    await server.close();
  }
});
