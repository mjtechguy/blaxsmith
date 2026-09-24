import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { createServer } from "vite";

let server;
let m;
before(async () => {
  server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  m = await server.ssrLoadModule("/src/palette.ts");
});
after(() => server.close());

const data = {
  projects: [{ id: "p1", name: "Portal", slug: "portal" }, { id: "p2", name: "Billing", slug: "billing" }],
  runs: [{ id: "r1", projectId: "p1", launchKey: "export-fix", state: "active" }],
  recipes: [{ id: "rc1", name: "Guild engineering", projectId: "", currentVersionId: "v1" }, { id: "rc2", name: "Draft", projectId: "p1", currentVersionId: "" }],
  connections: [
    { id: "c-org", scope: "organization", ownerId: "o", provider: "anthropic", kind: "api_key", label: "Anthropic prod" },
    { id: "c-me", scope: "personal", ownerId: "u", provider: "codex", kind: "subscription", label: "" },
  ],
  users: [{ principalId: "u2", username: "mara", displayName: "Mara Lin", role: "member" }],
  inbox: [{ id: "ix1", runId: "r1", projectId: "p1", projectName: "Portal", title: "Approve crew plan", kind: "approval" }],
};
const ids = (items) => items.map((i) => i.id);

test("owners and admins see everything, including users and org connections", () => {
  for (const role of ["owner", "admin"]) {
    const items = ids(m.paletteItems(role, data, { projectId: "p1" }));
    for (const id of ["user:u2", "connection:c-org", "connection:c-me", "inbox:ix1", "action:connect-github", "action:new-org-api-key", "action:new-project", "action:run-recipe:rc1"]) {
      assert.ok(items.includes(id), `${role} missing ${id}`);
    }
  }
});

test("members get no users, org connections, admin actions, or admin inbox", () => {
  const items = m.paletteItems("member", data, { projectId: "p1" });
  const got = ids(items);
  for (const id of ["user:u2", "connection:c-org", "inbox:ix1", "action:connect-github", "action:new-org-api-key"]) assert.ok(!got.includes(id), `member sees ${id}`);
  for (const id of ["connection:c-me", "project:p1", "run:r1", "action:new-project", "action:run-recipe:rc1", "action:inbox"]) assert.ok(got.includes(id), `member missing ${id}`);
  assert.ok(!got.includes("action:run-recipe:rc2"), "recipe without a current version offered for launch");
  assert.ok(items.every((i) => !i.to.startsWith("/admin")), "member offered an admin route");
});

test("viewers cannot create projects or start runs", () => {
  const got = ids(m.paletteItems("viewer", data, { projectId: "p1" }));
  assert.ok(!got.some((id) => id === "action:new-project" || id.startsWith("action:run-")));
  assert.ok(got.includes("project:p1") && got.includes("action:new-api-key"));
});

test("stored recents are filtered by role and lead the empty query", () => {
  const recents = [{ id: "user:u2", group: "Users", label: "Mara", to: "/admin/users" }, { id: "project:p1", group: "Projects", label: "Portal", to: "/projects/p1" },
    { id: "action:new-project", group: "Actions", label: "New project", to: "/projects/new" }];
  const items = m.paletteItems("viewer", data);
  const shown = m.filterPalette(items, recents, "", "viewer");
  assert.deepEqual(ids(shown.filter((i) => i.group === "Recent")), ["project:p1"]);
  const admin = m.filterPalette(m.paletteItems("owner", data), recents, "", "owner");
  assert.deepEqual(ids(admin.slice(0, 3)), ["user:u2", "project:p1", "action:new-project"]);
  assert.equal(admin.filter((i) => i.id === "action:new-project").length, 1);
});

test("queries rank and group matches; recents keep at most 12", () => {
  const items = m.paletteItems("owner", data, { projectId: "p1" });
  const shown = m.filterPalette(items, [], "port", "owner");
  assert.ok(shown.some((i) => i.id === "project:p1"));
  assert.equal(m.filterPalette(items, [], "zzzz", "owner").length, 0);
  assert.equal(m.matchScore({ id: "x", group: "Projects", label: "Portal", to: "/" }, "por"), 0);
  assert.equal(m.matchScore({ id: "x", group: "Projects", label: "Portal", to: "/" }, "tal"), 4);
  let recents = [];
  for (let i = 0; i < 20; i++) recents = m.addRecent(recents, { id: `project:${i}`, group: "Projects", label: `P${i}`, to: `/projects/${i}` });
  assert.equal(recents.length, 12);
  assert.equal(recents[0].id, "project:19");
  recents = m.addRecent(recents, { ...recents[5], group: "Recent" });
  assert.equal(recents[0].group, "Projects");
});
