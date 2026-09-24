import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

const session = (role) => ({ organizationId: "00000000-0000-4000-8000-000000000001", principalId: "00000000-0000-4000-8000-000000000002",
  sessionId: "00000000-0000-4000-8000-000000000003", role, username: `${role}-user`, accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() });
const adminLinks = (html) => [...html.matchAll(/<nav aria-label="Main">(.*?)<\/nav>/gs)].flatMap((m) => [...m[1].matchAll(/href="(\/admin[^"]*)"/g)].map((l) => l[1]));

test("admin is one guarded section and the sidebar has a single Admin item", async () => {
  const saved = { window: globalThis.window, fetch: globalThis.fetch, localStorage: globalThis.localStorage, matchMedia: globalThis.matchMedia };
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  globalThis.localStorage = { getItem: () => null, setItem: () => {} };
  globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
  const requests = [];
  globalThis.fetch = async (input) => { requests.push(String(input)); return new Response("{}", { status: 200, headers: { "content-type": "application/json" } }); };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { renderApp } = await server.ssrLoadModule("/tests/render-app.tsx");
    for (const role of ["member", "viewer"]) {
      for (const path of ["/admin/recipes", "/admin/connections/new/git", "/admin", "/admin/users"]) {
        const html = await renderApp(path, session(role));
        assert.match(html, /Administration is restricted/, `${role} at ${path}`);
        assert.doesNotMatch(html, /aria-label="Administration"/, `${role} saw the admin subnav at ${path}`);
        assert.doesNotMatch(html, /Connect Git|Recipe library|Organization recipes/, `${role} saw admin page content at ${path}`);
      }
      assert.deepEqual(adminLinks(await renderApp("/", session(role))), [], `${role} sidebar shows admin`);
    }
    for (const role of ["owner", "admin"]) {
      const html = await renderApp("/admin/connections/new/git", session(role));
      assert.doesNotMatch(html, /Administration is restricted/);
      assert.match(html, /Connect Git/);
      assert.match(html, /<nav class="admin-subnav" aria-label="Administration">/);
      for (const label of ["Operations", "Users", "Connections", "Recipes", "Audit", "Settings"]) assert.match(html, new RegExp(`>${label}</a>`));
      assert.deepEqual([...html.matchAll(/<a ([^>]*)>([^<]+)<\/a>/g)].filter((m) => m[1].includes("admin-subnav-link") && m[1].includes('aria-current="page"')).map((m) => m[2]), ["Connections"]);
      assert.deepEqual(adminLinks(await renderApp("/", session(role))), ["/admin"], `${role} sidebar`);
    }
    const { adminSectionFor } = await server.ssrLoadModule("/src/admin.ts");
    assert.deepEqual(["/admin", "/admin/", "/admin/users/new", "/admin/connections/abc", "/admin/connections/github-app", "/admin/recipes/r1/versions/new", "/admin/audit"].map(adminSectionFor),
      ["/admin", "/admin", "/admin/users", "/admin/connections", "/admin/connections/github-app", "/admin/recipes", "/admin/audit"]);
  } finally {
    await server.close();
    Object.assign(globalThis, saved);
  }
});
