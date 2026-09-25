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

const session = (role) => ({ organizationId: "00000000-0000-4000-8000-000000000001", principalId: "00000000-0000-4000-8000-000000000002",
  sessionId: "00000000-0000-4000-8000-000000000003", role, accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() });

async function withApp(run) {
  const saved = { window: globalThis.window, fetch: globalThis.fetch, localStorage: globalThis.localStorage, matchMedia: globalThis.matchMedia };
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  globalThis.localStorage = { getItem: () => null, setItem: () => {} };
  globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
  globalThis.fetch = async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    await run(await server.ssrLoadModule("/tests/render-app.tsx"), server);
  } finally {
    await server.close();
    Object.assign(globalThis, saved);
  }
}

test("personal connections are open to every role; project ones follow the server's project-admin answer", async () => {
  await withApp(async ({ renderApp }) => {
    const org = session("viewer").organizationId;
    const personal = [["connections", org, "personal", ""], []];
    for (const role of ["viewer", "member", "admin", "owner"]) {
      const html = await renderApp("/me/connections", session(role), [personal]);
      assert.match(html, /href="\/me\/connections\/new\/subscription"/, `${role} can add a subscription`);
      assert.match(html, /href="\/me\/connections\/new\/api-key"/, `${role} can add a personal key`);
    }
    const project = (canAdminister) => [["project", org, "p1"], { project: { id: "p1", slug: "p", name: "Payments", createdAt: "" }, canAdminister, canLaunch: true }];
    const lists = [[["connections", org, "project", "p1"], []], [["connections", org, "project_available", "p1"], []]];
    const member = await renderApp("/projects/p1/connections", session("member"), [project(false), ...lists]);
    assert.doesNotMatch(member, /connections\/new\/(api-key|git)/, "a member who does not administer the project gets no create actions");
    assert.match(member, /managed by project admins/);
    const creator = await renderApp("/projects/p1/connections", session("member"), [project(true), ...lists]);
    assert.match(creator, /href="\/projects\/p1\/connections\/new\/api-key"/, "the project's admin can add a key");
    assert.match(await renderApp("/projects/p1/connections/new/api-key", session("member"), [project(false)]), /Adding project connections is restricted/);
  });
});

test("connection titles drop the provider when the label already names it", async () => {
  await withApp(async (_app, server) => {
    const { connectionTitle } = await server.ssrLoadModule("/src/connections.ts");
    assert.equal(connectionTitle({ provider: "openai", label: "OpenAI sandbox" }), "OpenAI sandbox");
    assert.equal(connectionTitle({ provider: "openai", label: "Evaluation" }), "OpenAI · Evaluation");
    assert.equal(connectionTitle({ provider: "anthropic", label: "" }), "Anthropic");
  });
});

test("unknown detail IDs render one shared not-found page with an h1 and a way back", async () => {
  await withApp(async ({ renderApp }) => {
    const org = session("owner").organizationId;
    const html = await renderApp("/me/connections/nope", session("owner"), [[["connections", org, "personal", ""], []]]);
    assert.match(html, /<h1>Connection not found<\/h1>/);
    assert.match(html, /href="\/me\/connections"[^>]*>.*My connections/s);
    const orgConnection = await renderApp("/admin/connections/nope", session("owner"), [[["connections", org, "organization", ""], []]]);
    assert.match(orgConnection, /<h1>Connection not found<\/h1>/);
    assert.match(await renderApp("/no/such/page", session("owner")), /<h1>Page not found<\/h1>/);
  });
});

test("the project setup checklist lists only the steps each person can take", async () => {
  await withApp(async ({ renderApp }) => {
    const org = session("owner").organizationId;
    const seeds = (canAdminister, canLaunch) => [
      [["project", org, "p1"], { project: { id: "p1", slug: "p", name: "Payments", createdAt: "2026-09-01T00:00:00Z" }, canAdminister, canLaunch }],
      [["project-source", org, "p1"], null], [["project-verification", org, "p1"], null],
      [["recipes", org, "p1"], { recipes: [] }], [["project-model-access", org, "p1"], { access: [] }],
      [["runs", org, "p1", "checklist"], { runs: [] }],
    ];
    const steps = (html) => [...html.matchAll(/<li[^>]*>.*?<strong>([^<]+)<\/strong>/gs)].map((m) => m[1])
      .filter((label) => /repository|checks|recipe|model access|first run/.test(label));
    assert.deepEqual(steps(await renderApp("/projects/p1", session("owner"), seeds(true, true))),
      ["Connect the repository", "Set verification checks", "Make a recipe available", "Give runs model access", "Start the first run"]);
    assert.deepEqual(steps(await renderApp("/projects/p1", session("member"), seeds(true, true))), ["Give runs model access", "Start the first run"], "project admin member");
    const member = await renderApp("/projects/p1", session("member"), seeds(false, true));
    assert.deepEqual(steps(member), ["Start the first run"], "member without project admin");
    assert.match(member, /An admin still has to finish this project/);
    const viewer = await renderApp("/projects/p1", session("viewer"), seeds(false, false));
    assert.doesNotMatch(viewer, /Set up this project|>Setup</, "viewers get no checklist or Setup tile");
  });
});

test("grants name users by label and never show a bare principal id", async () => {
  await withApp(async (_app, server) => {
    const { granteeLabel } = await server.ssrLoadModule("/src/connections.ts");
    assert.equal(granteeLabel({ granteeKind: "user", granteeId: "p-mara", granteeName: "Mara Lin" }), "Mara Lin");
    assert.equal(granteeLabel({ granteeKind: "user", granteeId: "0f3c9a1e-1111-4000-8000-000000000000", granteeName: "" }), "Unknown user · 0f3c9a1e");
    assert.equal(granteeLabel({ granteeKind: "role", granteeId: "member", granteeName: "" }, " and above"), "member and above");
    assert.equal(granteeLabel({ granteeKind: "project", granteeId: "", granteeName: "", projectName: "Payments" }), "Payments");
  });
});
