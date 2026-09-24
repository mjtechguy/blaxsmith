import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

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

test("moved pages redirect from their old URLs", async () => {
  await withApp(async ({ renderApp }, server) => {
    // Each old route's beforeLoad throws a redirect to the new location.
    const redirectOf = async (file, params) => {
      const { Route } = await server.ssrLoadModule(`/src/routes/${file}`);
      try { await Route.options.beforeLoad({ params, location: { pathname: "" } }); } catch (thrown) {
        return thrown.options.to.replace("$projectId", thrown.options.params?.projectId ?? "").replace("$recipeId", thrown.options.params?.recipeId ?? "");
      }
      return null;
    };
    for (const [file, params, to] of [
      ["projects.$projectId.source.tsx", { projectId: "p1" }, "/projects/p1/settings/source"],
      ["projects.$projectId.verification.tsx", { projectId: "p1" }, "/projects/p1/settings/verification"],
      ["projects.$projectId.model-access.new.tsx", { projectId: "p1" }, "/projects/p1/connections"],
      ["admin.connections.github-app.tsx", {}, "/admin/settings/github-app"],
      // Admin › Recipes folded into Library › Recipes, which keeps the admin actions.
      ["admin.recipes.index.tsx", {}, "/recipes"],
      ["admin.recipes.new.tsx", {}, "/recipes/new"],
      ["admin.recipes.$recipeId.index.tsx", { recipeId: "rc1" }, "/recipes/rc1"],
      ["admin.recipes.$recipeId.versions.new.tsx", { recipeId: "rc1" }, "/recipes/rc1/versions/new"],
      ["admin.settings.index.tsx", {}, "/admin/settings/github-app"],
      ["me.settings.index.tsx", {}, "/me/settings/profile"],
    ]) assert.equal(await redirectOf(file, params), to, file);
    // Routes that did not move still render inside the shell.
    const owner = session("owner");
    for (const path of ["/", "/inbox", "/runs", "/projects", "/projects/new", "/projects/p1", "/projects/p1/runs", "/recipes", "/recipes/rc1", "/recipes/new", "/recipes/rc1/versions/new", "/tools", "/admin/users/u1", "/admin/settings/policies"]) {
      assert.doesNotMatch(await renderApp(path, owner), /Page not found/, path);
    }
  });
});

test("templates: settings sections, create flow steps, detail tabs, and account menu", async () => {
  await withApp(async ({ renderApp, renderDetailFixture, renderChecklist }) => {
    const owner = session("owner");
    const settings = await renderApp("/projects/p1/settings/source", owner);
    assert.match(settings, /<nav class="settings-nav" aria-label="Settings sections">/);
    assert.match(settings, /aria-current="page"[^>]*><span>Git source<\/span>/);
    const flow = await renderApp("/projects/new", owner);
    assert.match(flow, /aria-label="Step 1 of 5"/);
    assert.match(flow, /aria-current="step"/);
    assert.equal([...flow.matchAll(/class="flow-step is-todo"/g)].length, 4);

    // Account menu replaces the old topbar sign-out; the sidebar has no account items.
    const home = await renderApp("/", owner);
    assert.match(home, /aria-haspopup="menu"/);
    assert.doesNotMatch(home.match(/<aside[^>]*>.*?<\/aside>/s)[0], /Sign out|My connections/);
    // Account settings sections are real forms now (see account.test.mjs).
    for (const section of ["profile", "password", "sessions"]) {
      assert.doesNotMatch(await renderApp(`/me/settings/${section}`, owner), /Coming soon|Page not found/, section);
    }
    assert.match(await renderApp("/me/settings/preferences", owner), /type="radio"/);

    // Setup checklists list what is left and fold finished steps into one disclosure.
    const partial = await renderChecklist([{ id: "a", done: true }, { id: "b", done: false }, { id: "c", done: true }]);
    assert.match(partial, /2 of 3 done · 1 step left/);
    assert.match(partial, /Step b/);
    assert.doesNotMatch(partial, /Step a|Step c/, "finished steps start collapsed");
    assert.match(partial, /aria-expanded="false"[^>]*>.*?2<!-- --> done · <!-- -->Show/s);
    const complete = await renderChecklist([{ id: "a", done: true }, { id: "b", done: true }]);
    assert.match(complete, /Setup complete/);
    assert.match(complete, /aria-label="Dismiss Set up this project"/);
    assert.doesNotMatch(complete, /Step a|<ol/, "a finished checklist is one compact line");
    assert.doesNotMatch(await renderChecklist([{ id: "a", done: true }, { id: "b", done: undefined }]), /Setup complete/, "not complete while loading");

    const detail = await renderDetailFixture();
    assert.match(detail, /<nav class="route-tabs" aria-label="Run sections">/);
    assert.match(detail, /href="\/projects\/p\/runs\/r\?tab=stages"[^>]*aria-current="page"|aria-current="page"[^>]*href="\/projects\/p\/runs\/r\?tab=stages"/);
    assert.doesNotMatch(detail, />Review</, "hidden tabs are not rendered");
    assert.match(detail, /<dt>Commit<\/dt><dd>abc<\/dd>/);
    assert.match(detail, /aria-expanded="false"[^>]*>.*Advanced/s);
    assert.doesNotMatch(detail, /hidden body/, "disclosures start collapsed");
    assert.match(detail, /<code>01234567…<\/code>/);
  });
});
