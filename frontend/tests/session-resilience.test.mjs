import assert from "node:assert/strict";
import { after, before, beforeEach, test } from "node:test";
import { createServer } from "vite";

// One fake server for the whole file: route handlers by RPC method name.
let handlers = {};
const calls = [];
const json = (status, body) => new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
const unauthenticated = () => json(401, { code: "unauthenticated", message: "authentication required" });
const session = { organizationId: "org", principalId: "user", role: "owner", accessExpiresAt: new Date(Date.now() + 600_000).toISOString() };
const policy = { idleTimeoutSeconds: "604800", absoluteLifetimeSeconds: "2592000", accessTokenSeconds: "600", refreshGraceSeconds: "60" };

let server;
let auth;
let connectivity;
let resilience;
let admin;
let nav;
const saved = {};
const clients = [];
const track = (client) => { clients.push(client); return client; };

before(async () => {
  Object.assign(saved, { window: globalThis.window, fetch: globalThis.fetch, navigator: globalThis.navigator });
  globalThis.window = { location: { origin: "https://blaxsmith.test" }, setTimeout, clearTimeout };
  let release = Promise.resolve();
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: { locks: {
    async request(_name, _options, action) {
      const previous = release;
      let unlock;
      release = new Promise((resolve) => { unlock = resolve; });
      await previous;
      try { return await action(); } finally { unlock(); }
    },
  } } });
  globalThis.fetch = async (input) => {
    const method = String(input).split("/").at(-1);
    calls.push(method);
    const handler = handlers[method];
    if (!handler) return json(404, { code: "unimplemented", message: method });
    return handler();
  };
  server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  auth = await server.ssrLoadModule("/src/auth.ts");
  connectivity = await server.ssrLoadModule("/src/connectivity.ts");
  resilience = await server.ssrLoadModule("/src/resilience.ts");
  admin = await server.ssrLoadModule("/src/admin.ts");
  nav = await server.ssrLoadModule("/src/nav.ts");
});

after(async () => {
  connectivity.reportReachable();
  for (const client of clients) client.clear(); // query gc timers would hold the process open
  await server.close();
  globalThis.window = saved.window;
  globalThis.fetch = saved.fetch;
  Object.defineProperty(globalThis, "navigator", { configurable: true, value: saved.navigator });
});

beforeEach(() => {
  calls.length = 0;
  handlers = { GetCsrf: () => json(200, { token: "A".repeat(43) }) };
  connectivity.reportReachable();
});

const count = (method) => calls.filter((c) => c === method).length;
const sequence = (...responses) => { let i = 0; return () => responses[Math.min(i++, responses.length - 1)](); };

test("outages are told apart from sign-outs, with capped jittered backoff", async () => {
  const { Code, ConnectError } = await server.ssrLoadModule("@connectrpc/connect");
  const { isUnavailable, backoffDelay } = connectivity;
  assert.equal(isUnavailable(new TypeError("Failed to fetch")), true);
  assert.equal(isUnavailable(new ConnectError("down", Code.Unavailable)), true);
  assert.equal(isUnavailable(new ConnectError("slow", Code.DeadlineExceeded)), true);
  assert.equal(isUnavailable(ConnectError.from(new TypeError("Failed to fetch"))), true);
  for (const code of [Code.Unauthenticated, Code.PermissionDenied, Code.Internal, Code.NotFound, Code.Canceled]) {
    assert.equal(isUnavailable(new ConnectError("no", code)), false, Code[code]);
  }
  assert.equal(backoffDelay(0, () => 0), 500);
  assert.equal(backoffDelay(0, () => 1), 1_000);
  assert.equal(backoffDelay(3, () => 1), 8_000);
  assert.equal(backoffDelay(40, () => 0), 15_000, "capped, half fixed");
  assert.equal(backoffDelay(40, () => 1), 30_000, "capped");
});

test("Unavailable keeps you signed in and retries until the server is back", async () => {
  const queryClient = track(resilience.createAppQueryClient());
  queryClient.setQueryData(auth.sessionQueryKey, session);
  const states = [];
  const stop = connectivity.subscribeConnection(() => states.push(connectivity.connectionState()));
  let reconnected = 0;
  const off = connectivity.onReconnected(() => reconnected++);
  // A restarting pod behind the ingress: 503, then a dropped connection, then back.
  handlers.GetSessionPolicy = sequence(() => json(503, { code: "unavailable", message: "restarting" }),
    () => { throw new TypeError("Failed to fetch"); }, () => json(200, policy));
  // The session check during the outage fails as unavailable, never as signed out.
  handlers.CurrentSession = () => json(502, {});
  const outage = await auth.currentSession().then(() => "resolved", (error) => error);
  assert.notEqual(outage, "resolved");
  assert.equal(connectivity.isUnavailable(outage), true);
  try {
    const result = await queryClient.fetchQuery({ queryKey: ["policy"], queryFn: ({ signal }) => admin.getSessionPolicy(signal), retryDelay: 1 });
    assert.equal(result.idleTimeoutSeconds, 604800n);
  } finally {
    stop();
    off();
  }
  assert.equal(count("GetSessionPolicy"), 3, "retried through the outage");
  assert.equal(count("RefreshSession"), 0, "an outage never spends the refresh token");
  assert.deepEqual(states, ["reconnecting", "online"], "banner shown, then cleared");
  assert.equal(reconnected, 1);
  assert.deepEqual(queryClient.getQueryData(auth.sessionQueryKey), session, "still signed in");
  // Mutations retry an outage too, a bounded number of times.
  assert.equal(queryClient.getDefaultOptions().mutations.retry(0, new TypeError("Failed to fetch")), true);
  assert.equal(queryClient.getDefaultOptions().mutations.retry(resilience.MUTATION_RETRIES, new TypeError("Failed to fetch")), false);
  const { Code, ConnectError } = await server.ssrLoadModule("@connectrpc/connect");
  assert.equal(queryClient.getDefaultOptions().queries.retry(0, new ConnectError("no", Code.PermissionDenied)), false);
});

test("Unauthenticated triggers exactly one refresh and a retry", async () => {
  let accessValid = false;
  handlers.CurrentSession = () => accessValid ? json(200, { session }) : unauthenticated();
  handlers.RefreshSession = () => { accessValid = true; return json(200, { session }); };
  handlers.GetSessionPolicy = () => accessValid ? json(200, policy) : unauthenticated();
  // Three requests hit the expired access cookie at once.
  const results = await Promise.all([admin.getSessionPolicy(), admin.getSessionPolicy(), admin.getSessionPolicy()]);
  for (const result of results) assert.equal(result.accessTokenSeconds, 600n);
  assert.equal(count("RefreshSession"), 1, "one refresh for all of them");
  assert.equal(count("GetSessionPolicy"), 6, "each request retried exactly once");

  // A request that is still refused after a good refresh is not retried again.
  calls.length = 0;
  handlers.GetSessionPolicy = unauthenticated;
  let expired = 0;
  const off = auth.onSessionExpired(() => expired++);
  try {
    await assert.rejects(admin.getSessionPolicy(), /authentication required/);
  } finally {
    off();
  }
  assert.equal(count("GetSessionPolicy"), 2);
  assert.equal(count("RefreshSession"), 0, "the access cookie was already fresh");
  assert.equal(expired, 0, "the session itself is still valid");
});

test("the access token is renewed shortly before it expires", async () => {
  const soon = { ...session, accessExpiresAt: new Date(Date.now() + 30_000).toISOString() };
  let renewed = false;
  handlers.CurrentSession = () => json(200, { session: renewed ? session : soon });
  handlers.RefreshSession = () => { renewed = true; return json(200, { session }); };
  const current = await auth.currentSession();
  assert.equal(current.accessExpiresAt, session.accessExpiresAt);
  assert.equal(count("RefreshSession"), 1);
});

test("a failed refresh sends you to /login with a return path", async () => {
  const queryClient = track(resilience.createAppQueryClient());
  queryClient.setQueryData(auth.sessionQueryKey, session);
  handlers.CurrentSession = unauthenticated;
  handlers.RefreshSession = unauthenticated;
  handlers.GetSessionPolicy = unauthenticated;
  await assert.rejects(admin.getSessionPolicy(), /authentication required/);
  assert.equal(count("RefreshSession"), 1);
  assert.equal(count("GetSessionPolicy"), 1, "no retry without a session");
  assert.equal(queryClient.getQueryData(auth.sessionQueryKey), null, "session cleared, so the root layout routes to sign-in");
  assert.equal(connectivity.connectionState(), "online", "a sign-out is not an outage");
  assert.deepEqual(nav.loginTarget("/projects/p1/runs/r1?tab=review"), { to: "/login", search: { next: "/projects/p1/runs/r1?tab=review" } });
  assert.deepEqual(nav.loginTarget("https://evil.test/"), { to: "/login", search: { next: "/" } });
});

test("live channels back off with jitter and reconnect at once after a drain", async () => {
  const { socketRetryDelay } = await server.ssrLoadModule("/src/live.ts");
  assert.ok(socketRetryDelay(1001, 5, () => 1) <= 750, "1001 going away reconnects promptly");
  assert.equal(socketRetryDelay(1006, 0, () => 0), 500);
  assert.equal(socketRetryDelay(1006, 20, () => 1), 30_000);
});
