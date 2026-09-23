import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("concurrent tabs recheck the access cookie before rotating refresh", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const previousWindow = globalThis.window;
  const previousNavigator = globalThis.navigator;
  const previousFetch = globalThis.fetch;
  let accessValid = false;
  let refreshCalls = 0;
  let release = Promise.resolve();
  const session = { organizationId: "org", principalId: "user", role: "owner", accessExpiresAt: new Date(Date.now() + 600_000).toISOString() };

  globalThis.window = { location: { origin: "https://blaxsmith.test" }, setTimeout, clearTimeout };
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
    let status = 200;
    let body = {};
    if (method === "CurrentSession") {
      if (accessValid) body = { session };
      else { status = 401; body = { code: "unauthenticated", message: "authentication required" }; }
    } else if (method === "GetCsrf") body = { token: "A".repeat(43) };
    else if (method === "RefreshSession") {
      refreshCalls++;
      if (refreshCalls === 1) { accessValid = true; body = { session }; }
      else { status = 401; body = { code: "unauthenticated", message: "reused refresh" }; }
    }
    return new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json" } });
  };

  try {
    const { currentSession } = await server.ssrLoadModule("/src/auth.ts");
    const [first, second] = await Promise.all([currentSession(), currentSession()]);
    assert.equal(first.principalId, "user");
    assert.equal(second.principalId, "user");
    assert.equal(refreshCalls, 1);
    accessValid = false;
    globalThis.navigator.locks = undefined;
    await assert.rejects(currentSession(), /Secure session coordination is unavailable/);
    assert.equal(refreshCalls, 1);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    Object.defineProperty(globalThis, "navigator", { configurable: true, value: previousNavigator });
    globalThis.fetch = previousFetch;
  }
});
