import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

const shape = (groups) => groups.map((g) => [g.id, g.items.map((i) => i.children ? [i.label, i.children.map((c) => c.label)] : i.label)]);

test("navigation IA per role: groups, items, the project group, and Admin", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { navigation, activeItem, breadcrumbs, detailKind, projectIdFrom, switchPath } = await server.ssrLoadModule("/src/nav.ts");
    const common = [
      ["home", ["Home"]],
      ["work", ["Inbox", "Runs", "Projects"]],
      ["library", ["Recipes", "Extensions", "Tools & runtimes"]],
    ];
    // Organization recipes live once, in Library; Admin has no Recipes item.
    const admin = ["admin", ["Operations", "Users", "Connections", "Extensions", "Audit", ["Settings", ["GitHub app", "Model gateway", "Connections", "Policies", "Retention"]]]];
    // Usage & gateway and Routes & pools appear only while the model gateway master switch is on.
    assert.deepEqual(shape(navigation({ role: "owner", gatewayEnabled: true })).at(-1),
      ["admin", ["Operations", "Users", "Connections", "Extensions", "Usage & gateway", "Routes & pools", "Budgets", "Alerts", "Audit", ["Settings", ["GitHub app", "Model gateway", "Connections", "Policies", "Retention"]]]]);
    assert.deepEqual(shape(navigation({ role: "member", gatewayEnabled: true })), common);
    assert.deepEqual(shape(navigation({ role: "member" })), common);
    assert.deepEqual(shape(navigation({ role: "viewer" })), common);
    assert.deepEqual(shape(navigation({ role: "admin" })), [...common, admin]);
    assert.deepEqual(shape(navigation({ role: "owner" })), [...common, admin]);
    // Signed out: only the public catalog.
    assert.deepEqual(shape(navigation({})), [["library", ["Tools & runtimes"]]]);
    // My connections lives in the account menu, not the sidebar.
    assert.ok(!JSON.stringify(shape(navigation({ role: "owner", projectId: "p" }))).includes("My connections"));

    // The project group appears only with a project in the URL, after Work.
    const withProject = navigation({ role: "member", projectId: "p1", projectName: "Billing" });
    assert.deepEqual(shape(withProject)[2], ["project", ["Overview", "Runs", "Project recipes", "Project connections", "Source & verification", "Settings"]]);
    assert.equal(withProject[2].label, "Billing");
    // Project → Usage appears only while the model gateway is on.
    assert.deepEqual(shape(navigation({ role: "member", projectId: "p1", gatewayEnabled: true }))[2],
      ["project", ["Overview", "Runs", "Usage", "Project recipes", "Project connections", "Source & verification", "Settings"]]);
    assert.equal(projectIdFrom("/projects/p1/runs/r1"), "p1");
    assert.equal(projectIdFrom("/projects/new"), undefined);
    assert.equal(projectIdFrom("/projects"), undefined);

    // Most specific item wins; parent and child highlight predictably.
    const owner = navigation({ role: "owner", projectId: "p1" });
    const at = (path) => { const a = activeItem(owner, path); return a ? [a.group.id, a.item.label, a.parent?.label ?? null] : null; };
    assert.deepEqual(at("/"), ["home", "Home", null]);
    assert.deepEqual(at("/admin"), ["admin", "Operations", null]);
    assert.deepEqual(at("/admin/users/abc"), ["admin", "Users", null]);
    assert.deepEqual(at("/admin/settings/github-app"), ["admin", "GitHub app", "Settings"]);
    assert.deepEqual(at("/projects/p1"), ["project", "Overview", null]);
    assert.deepEqual(at("/projects/p1/setup"), ["project", "Overview", null]);
    assert.deepEqual(at("/projects/p1/runs/r9"), ["project", "Runs", null]);
    assert.deepEqual(at("/projects/p1/settings"), ["project", "Settings", null]);
    assert.deepEqual(at("/projects/p1/settings/verification"), ["project", "Source & verification", null]);
    assert.deepEqual(at("/projects/p1/model-access"), ["project", "Project connections", null]);
    assert.deepEqual(at("/projects/new"), ["work", "Projects", null]);
    assert.deepEqual(at("/recipes/r1"), ["library", "Recipes", null]);
    assert.deepEqual(at("/recipes/new"), ["library", "Recipes", null]);
    assert.deepEqual(at("/recipes/r1/versions/new"), ["library", "Recipes", null]);
    // "Recipes" and "Connections" each read distinctly across the sidebar.
    const labelsOf = (groups) => groups.flatMap((g) => g.items.map((i) => i.label));
    const all = labelsOf(owner);
    assert.equal(all.filter((l) => l === "Recipes").length, 1);
    assert.equal(all.filter((l) => /Connections$/i.test(l)).length, 2);
    assert.ok(all.includes("Project connections") && !all.includes("My connections"));
    assert.equal(at("/me/settings"), null);

    // Breadcrumbs follow the hierarchy.
    const labels = (path, detail) => breadcrumbs(owner, path, detail).map((c) => c.label);
    assert.deepEqual(labels("/admin/settings/policies"), ["Admin", "Settings", "Policies"]);
    assert.deepEqual(labels("/projects/p1/runs/r9", "guild-demo"), ["Project", "Runs", "guild-demo"]);
    assert.deepEqual(labels("/me/settings/profile"), ["Account", "Account settings"]);
    assert.deepEqual(breadcrumbs(owner, "/admin/users")[1], { label: "Users", href: undefined });
    assert.equal(detailKind("/projects/p1/runs/r9"), "run");
    assert.equal(detailKind("/projects/p1/runs/new"), "new-run");
    assert.equal(detailKind("/admin/connections/github-app"), undefined);

    // Switching projects keeps the section, not the detail.
    assert.equal(switchPath("/projects/a/runs/r1", "b"), "/projects/b/runs");
    assert.equal(switchPath("/projects/a/settings/source", "b"), "/projects/b/settings/source");
    assert.equal(switchPath("/projects/a/connections/c1", "b"), "/projects/b/connections");
    assert.equal(switchPath("/inbox", "b"), "/projects/b");
    assert.equal(switchPath("/projects/a/usage", "b"), "/projects/b/usage");
  } finally {
    await server.close();
  }
});
