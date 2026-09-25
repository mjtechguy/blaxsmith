import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

// The app modules read these browser globals at import time.
globalThis.window = { location: { origin: "https://blaxsmith.test" } };
globalThis.localStorage = { getItem: () => null, setItem: () => {} };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
globalThis.fetch = async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } });

const org = "00000000-0000-4000-8000-000000000001";
const owner = { organizationId: org, principalId: "00000000-0000-4000-8000-000000000002", sessionId: "00000000-0000-4000-8000-000000000003", role: "owner", username: "owner", accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() };
const member = { ...owner, role: "member" };
const budget = (over = {}) => ({ id: "b1", name: "Billing monthly", scope: "project", projectId: "p1", projectName: "Billing", principalId: "", principalName: "",
  amountUsdMicros: 100_000_000n, thresholds: [50, 80, 100], version: 1n, spendUsdMicros: 85_000_000n, forecastUsdMicros: 140_000_000n, firedThresholds: [50, 80],
  periodStart: "2026-09-01", periodEnd: "2026-09-30", createdAt: "2026-09-02T10:00:00Z", ...over });
const alert = (over = {}) => ({ id: "a1", budgetId: "b1", budgetName: "Billing monthly", scope: "project", projectId: "p1", projectName: "Billing", principalId: "", principalName: "",
  thresholdPct: 80, spendUsdMicros: 80_400_000n, amountUsdMicros: 100_000_000n, forecastUsdMicros: 140_000_000n, periodStart: "2026-09-01", createdAt: "2026-09-20T10:00:00Z",
  acknowledgedAt: "", acknowledgedByUsername: "", snoozedUntil: "", ...over });
const totals = { requests: 10n, errors: 1n, rateLimited: 0n, inputTokens: 100n, outputTokens: 20n, cacheReadTokens: 50n, cacheWriteTokens: 0n, reasoningTokens: 0n, costUsdMicros: 85_000_000n };

test("budget helpers: thresholds, amounts, progress tone", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { parseThresholds, dollarsToMicros, budgetRatio, budgetTone } = await server.ssrLoadModule("/src/budgets.ts");
    assert.deepEqual(parseThresholds(""), [50, 80, 100]);
    assert.deepEqual(parseThresholds("100, 50 80"), [50, 80, 100]);
    for (const bad of ["0", "201", "50, 50", "1,2,3,4,5,6,7", "eighty", "12.5"]) assert.equal(parseThresholds(bad), null, bad);
    assert.equal(dollarsToMicros("$1,250.50"), 1_250_500_000n);
    assert.equal(dollarsToMicros("0"), null);
    assert.equal(dollarsToMicros(""), null);
    assert.equal(dollarsToMicros("-5"), null);
    assert.equal(budgetRatio(85n, 100n), 0.85);
    assert.equal(budgetRatio(1n, 0n), 0);
    assert.equal(budgetTone({ spendUsdMicros: 10n, amountUsdMicros: 100n, thresholds: [50, 80] }), "ok");
    assert.equal(budgetTone({ spendUsdMicros: 60n, amountUsdMicros: 100n, thresholds: [50, 80] }), "attention");
    assert.equal(budgetTone({ spendUsdMicros: 100n, amountUsdMicros: 100n, thresholds: [50, 80] }), "danger");
  } finally {
    await server.close();
  }
});

test("Admin → Budgets and Alerts, Project → Usage, inbox alerts and the restricted Cost tab", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const harness = await server.ssrLoadModule("/tests/render-app.tsx");
    // React separates adjacent text with comments; drop them so assertions read as text.
    const renderApp = async (...args) => (await harness.renderApp(...args)).replaceAll("<!-- -->", "");
    const renderInRouter = async (...args) => (await harness.renderInRouter(...args)).replaceAll("<!-- -->", "");
    const { budgetsKey, budgetAlertsKey, projectUsageKey } = await server.ssrLoadModule("/src/budgets.ts");
    const { gatewayStatusKey, runCostKey } = await server.ssrLoadModule("/src/gateway.ts");
    const { homeKey } = await server.ssrLoadModule("/src/workspace.ts");
    const on = [gatewayStatusKey(org), { enabled: true }];

    // Budgets: progress meter with forecast, fired thresholds, the alerts switch, the add form.
    const budgets = await renderApp("/admin/budgets", owner, [on, [budgetsKey(org), { gatewayEnabled: true, budgetsEnabled: true, budgets: [budget()] }]]);
    assert.match(budgets, /Billing monthly/);
    assert.match(budgets, /aria-label="\$85\.00 of \$100\.00 spent, 85%"/);
    assert.match(budgets, /at this rate \$140\.00 by Sep 30/);
    assert.match(budgets, /80% ✓/);
    assert.match(budgets, /Raise budget alerts/);
    assert.match(budgets, /Add budget/);
    assert.match(budgets, /Usage &amp; gateway/); // sidebar, with Budgets and Alerts after it
    assert.doesNotMatch(budgets, /style=/);
    // The gateway off: the page explains where to turn it on.
    const off = await renderApp("/admin/budgets", owner, [[budgetsKey(org), { gatewayEnabled: false, budgetsEnabled: false, budgets: [] }]]);
    assert.match(off, /The model gateway is off/);
    // Members never reach admin pages.
    assert.match(await renderApp("/admin/budgets", member), /Administration is restricted/);

    // Alerts: open alerts have acknowledge and snooze; acknowledged ones show who.
    const alerts = await renderApp("/admin/alerts", owner, [on, [budgetAlertsKey(org, false), { alerts: [alert(), alert({ id: "a2", thresholdPct: 50, acknowledgedAt: "2026-09-10T10:00:00Z", acknowledgedByUsername: "mara" })] }]]);
    assert.match(alerts, /Billing monthly passed 80%/);
    assert.match(alerts, />Acknowledge</);
    assert.match(alerts, /Snooze 1 day/);
    assert.match(alerts, /@mara/);

    // Project → Usage: a member sees spend and the budget, not spend by user, and cannot act on alerts.
    const usage = { enabled: true, fromDay: "2026-09-01", toDay: "2026-09-24", totals, budget: budget(), series: [{ day: "2026-09-02", key: "claude-opus-5-5", costUsdMicros: 5_000_000n, tokens: 0n }],
      byStage: [{ key: "implement", label: "implement", detail: "", totals }], byModel: [], byUser: [], byUserHidden: true, topRuns: [], alerts: [alert()] };
    const project = await renderApp("/projects/p1/usage", member, [on, [projectUsageKey(org, "p1"), usage]]);
    assert.match(project, /Spend this month/);
    assert.match(project, /Billing monthly: alerts at 50%, 80%, 100%/);
    assert.match(project, /visible to this project(&#x27;|')s administrators/);
    assert.match(project, /implement/);
    assert.doesNotMatch(project, />Acknowledge</);
    assert.match(project, /href="\/projects\/p1\/usage"/); // the project nav item

    // Home: a budget alert links to where its spend is shown.
    const home = { organizationName: "Acme", organizationSlug: "acme", username: "owner", displayName: "Owner", email: "", waitingOnYou: 1, openItems: 1, runningAgents: 0, activeRuns: 0, runsLast24h: 0, failedLast24h: 0,
      waiting: [{ id: "a1", kind: "budget_alert", runId: "", projectId: "p1", projectName: "Billing", runLaunchKey: "", stage: "project", title: "Billing monthly passed 80% of its monthly budget", blocking: false, createdAt: "2026-09-20T10:00:00Z", canAct: true }],
      agents: [], recentRuns: [], generatedAt: "2026-09-24T10:00:00Z" };
    const landing = await renderApp("/", owner, [on, [homeKey(`${org}:${owner.principalId}`), home]]);
    assert.match(landing, /Billing monthly passed 80% of its monthly budget/);
    assert.match(landing, /Budget alert · Billing</);
    assert.match(landing, /href="\/projects\/p1\/usage"/);

    // Cost tab on a personal-subscription run for a non-owner: aggregates only, never request rows.
    const React = await import("react");
    const { RunCostTab } = await server.ssrLoadModule("/src/run-cost.tsx");
    const cost = { enabled: true, totals, stages: [{ taskId: "t", stage: "implement", totals, cacheHitRatio: 0.3 }], requests: [], truncated: false, requestsRestricted: true };
    const restricted = await renderInRouter(React.createElement(RunCostTab, { scope: "s", runId: "r1" }), "/", [[runCostKey("s", "r1"), cost]]);
    assert.match(restricted, /Per stage/);
    assert.match(restricted, /personal subscription/);
    assert.doesNotMatch(restricted, /Model requests/);
    const full = await renderInRouter(React.createElement(RunCostTab, { scope: "s", runId: "r1" }), "/", [[runCostKey("s", "r1"), { ...cost, requestsRestricted: false }]]);
    assert.match(full, /Model requests/);
  } finally {
    await server.close();
  }
});
