import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

const session = (role) => ({ organizationId: "00000000-0000-4000-8000-000000000001", principalId: "00000000-0000-4000-8000-000000000002",
  sessionId: "00000000-0000-4000-8000-000000000003", role, username: `${role}-user`, accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() });
const sidebar = (html) => html.match(/<nav aria-label="Main">(.*?)<\/nav>/s)?.[1] ?? "";
const adminLinks = (html) => [...sidebar(html).matchAll(/href="(\/admin[^"]*)"/g)].map((l) => l[1]);
const currentLinks = (html) => [...sidebar(html).matchAll(/<a [^>]*aria-current="page"[^>]*>.*?<span class="nav-text">([^<]+)<\/span>/gs)].map((m) => m[1]);

test("admin is one guarded section; the sidebar carries its navigation", async () => {
  const saved = { window: globalThis.window, fetch: globalThis.fetch, localStorage: globalThis.localStorage, matchMedia: globalThis.matchMedia };
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  globalThis.localStorage = { getItem: () => null, setItem: () => {} };
  globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
  globalThis.fetch = async () => new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp } = await server.ssrLoadModule("/tests/render-app.tsx");
    for (const role of ["member", "viewer"]) {
      for (const path of ["/admin/recipes", "/admin/connections/new/git", "/admin", "/admin/users", "/admin/settings/policies"]) {
        const html = await renderApp(path, session(role));
        assert.match(html, /Administration is restricted/, `${role} at ${path}`);
        assert.doesNotMatch(html, /Connect Git|Recipe library|Organization recipes|Organization settings/, `${role} saw admin page content at ${path}`);
      }
      assert.deepEqual(adminLinks(await renderApp("/", session(role))), [], `${role} sidebar shows admin`);
    }
    for (const role of ["owner", "admin"]) {
      const html = await renderApp("/admin/connections/new/git", session(role));
      assert.doesNotMatch(html, /Administration is restricted/);
      assert.match(html, /Connect Git/);
      assert.doesNotMatch(html, /admin-subnav/, "the in-page admin subnav is gone");
      assert.deepEqual(currentLinks(html), ["Connections"], "the sidebar marks Admin › Connections current");
      assert.deepEqual(adminLinks(await renderApp("/", session(role))),
        ["/admin", "/admin/users", "/admin/connections", "/admin/recipes", "/admin/audit", "/admin/settings"], `${role} sidebar`);
      const settings = await renderApp("/admin/settings/retention", session(role));
      assert.match(settings, /Coming soon/);
      assert.doesNotMatch(settings, /<input/, "placeholder settings carry no fake form");
    }
  } finally {
    await server.close();
    Object.assign(globalThis, saved);
  }
});
