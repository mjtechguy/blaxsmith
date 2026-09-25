import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

// The app modules read these browser globals at import time.
globalThis.window = { location: { origin: "https://blaxsmith.test" } };
globalThis.localStorage = { getItem: () => null, setItem: () => {} };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
// Every RPC answers "gateway off" so gated UI must stay hidden.
globalThis.fetch = async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } });

test("gateway formatting, chart series and flag gating", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { usd, compact, percent, totalTokens } = await server.ssrLoadModule("/src/gateway.ts");
    assert.equal(usd(0n), "$0.00");
    assert.equal(usd(14_900_000n), "$14.90");
    assert.equal(usd(1_200n), "$0.0012"); // sub-cent spend is never shown as $0.00
    assert.equal(compact(12_900), "12.9K");
    assert.equal(percent(1, 3), "33%");
    assert.equal(percent(0, 0), "0%");
    assert.equal(totalTokens({ inputTokens: 1n, outputTokens: 2n, cacheReadTokens: 3n, cacheWriteTokens: 4n }), 10);

    const { buildSeries, niceMax, daysBetween, MAX_NAMED_SERIES } = await server.ssrLoadModule("/src/usage-chart.tsx");
    assert.equal(niceMax(0), 1);
    assert.equal(niceMax(7.3), 10);
    assert.equal(niceMax(1_200_000), 2_000_000);
    assert.deepEqual(daysBetween("2026-09-28", "2026-10-02"), ["2026-09-28", "2026-09-29", "2026-09-30", "2026-10-01", "2026-10-02"]);
    const points = ["a", "b", "c", "d", "e", "f", "g"].map((key, i) => ({ day: "2026-09-01", key, cost: (i + 1) * 10 }));
    const { series, slotOf } = buildSeries(points, new Map([["g", "Gamma"]]));
    // Five named series in fixed slot order by spend, then Other: no generated seventh hue.
    assert.equal(MAX_NAMED_SERIES, 5);
    assert.deepEqual(series.map((s) => [s.label, s.slot]), [["Gamma", 1], ["f", 2], ["e", 3], ["d", 4], ["c", 5], ["Other", 6]]);
    assert.equal(slotOf("a").key, "__other");
    assert.equal(buildSeries(points.slice(0, 2), new Map()).series.some((s) => s.key === "__other"), false);
  } finally {
    await server.close();
  }
});

test("gateway pages: settings always reachable for admins; other gateway UI hidden while off", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp, renderInRouter } = await server.ssrLoadModule("/tests/render-app.tsx");
    const owner = { organizationId: "00000000-0000-4000-8000-000000000001", principalId: "00000000-0000-4000-8000-000000000002", sessionId: "00000000-0000-4000-8000-000000000003", role: "owner", username: "owner", accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() };
    const settings = await renderApp("/admin/settings/model-gateway", owner);
    assert.match(settings, /Model gateway/);
    // A member sees the admin restriction, never the switches.
    const member = await renderApp("/admin/settings/model-gateway", { ...owner, role: "member" });
    assert.match(member, /Administration is restricted/);
    // Off (the status RPC reports disabled): no Usage & gateway in the sidebar.
    const home = await renderApp("/", owner);
    assert.match(home, /Model gateway|GitHub app|Operations/);
    assert.doesNotMatch(home, /Usage &amp; Gateway/);

    const React = await import("react");
    const { SpendChart } = await server.ssrLoadModule("/src/usage-chart.tsx");
    const html = await renderInRouter(React.createElement(SpendChart, {
      points: [{ day: "2026-09-01", key: "p1", cost: 2_000_000 }, { day: "2026-09-02", key: "p2", cost: 500_000 }],
      labels: new Map([["p1", "Billing"], ["p2", "Search"]]), from: "2026-09-01", to: "2026-09-03", title: "Estimated spend by project",
    }));
    // Legend for two series, focusable columns with an accessible value, a table toggle, and the estimate note.
    assert.match(html, /viz-legend/);
    assert.match(html, /Billing/);
    assert.match(html, /aria-label="Sep 1: \$2\.00 estimated"/);
    assert.match(html, /Show table/);
    assert.match(html, /Estimated USD/);
    assert.doesNotMatch(html, /style=/);
  } finally {
    await server.close();
  }
});
