import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("extension mutations send CSRF, install pins the previewed commit, and approval keeps required permissions", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "C".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    requests.push({ url: String(input).split("/api/")[1], body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") });
    return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const ext = await server.ssrLoadModule("/src/extensions.ts");
    const nav = await server.ssrLoadModule("/src/nav.ts");
    const source = { repositoryUrl: "https://github.com/example/guild", gitRef: "main", manifestPath: "", overlayManifestJson: "{}" };
    await ext.listExtensions();
    await ext.listExtensions("p1");
    await ext.previewExtensionInstall(source);
    await ext.installExtension(source, "a".repeat(40), ["mcp:foundry"]);
    await ext.checkExtensionUpdate("e1");
    await ext.grantExtension("e1", "p1", "project", "");
    await ext.revokeExtensionGrant("g1");
    const C = "C".repeat(43);
    assert.deepEqual(requests.map((r) => [r.url, r.csrf]), [
      ["blaxsmith.api.v1.ExtensionService/ListExtensions", null],
      ["blaxsmith.api.v1.ExtensionService/ListExtensions", null],
      ["blaxsmith.api.v1.ExtensionService/PreviewExtensionInstall", C],
      ["blaxsmith.api.v1.ExtensionService/InstallExtension", C],
      ["blaxsmith.api.v1.ExtensionService/CheckExtensionUpdate", C],
      ["blaxsmith.api.v1.ExtensionService/GrantExtension", C],
      ["blaxsmith.api.v1.ExtensionService/RevokeExtensionGrant", C],
    ]);
    assert.equal(requests[3].body.expectedCommit, "a".repeat(40));
    assert.deepEqual(requests[3].body.approvedPermissions, ["mcp:foundry"]);
    assert.equal(requests[3].body.source.overlayManifestJson, "{}");
    assert.deepEqual(requests[5].body, { extensionId: "e1", projectId: "p1", granteeKind: "project" });

    const permissions = [{ id: "mcp:foundry", optional: false }, { id: "mcp:serena", optional: true }, { id: "hook:foundry-serena", optional: true }];
    assert.deepEqual(ext.approvedPermissions(permissions, new Set()), ["mcp:foundry"]);
    assert.deepEqual(ext.approvedPermissions(permissions, new Set(["mcp:serena", "undeclared"])), ["mcp:foundry", "mcp:serena"]);
    // A project-scoped list asks the server for that project's grants only.
    assert.deepEqual(requests[1].body, { projectId: "p1" });
    // Admin > Extensions for owners/admins; Library > Extensions for every member.
    const owner = nav.navigation({ role: "owner" });
    const at = (groups, path) => { const a = nav.activeItem(groups, path); return a ? [a.group.id, a.item.label] : null; };
    for (const path of ["/admin/extensions", "/admin/extensions/new", "/admin/extensions/e1"]) assert.deepEqual(at(owner, path), ["admin", "Extensions"]);
    const member = nav.navigation({ role: "member" });
    assert.deepEqual(at(member, "/extensions/e1"), ["library", "Extensions"]);
    assert.ok(!member.some((g) => g.id === "admin"));
    assert.deepEqual(["/admin/extensions/new", "/admin/extensions/e1", "/extensions/e1", "/extensions"].map(nav.detailKind), ["new-extension", "extension", "extension", undefined]);

    // Recipe stages pick templates that fill their kind, from every installed version.
    const versions = [
      { version: "1.1.0", templates: [{ id: "forge-plan", title: "Forge plan", harness: "claude-code", kinds: ["plan", "interview"], reference: "guild@1.1.0/forge-plan" }] },
      { version: "1.0.0", templates: [{ id: "forge-plan", title: "", harness: "claude-code", kinds: ["plan"], reference: "guild@1.0.0/forge-plan" },
        { id: "review", title: "Review", harness: "claude-code", kinds: ["review"], reference: "guild@1.0.0/review" }] },
    ];
    assert.deepEqual(ext.templateChoices(versions, "1.1.0", "plan", "guild").map((c) => [c.reference, c.title, c.current]),
      [["guild@1.1.0/forge-plan", "Forge plan", true], ["guild@1.0.0/forge-plan", "forge-plan", false]]);
    assert.deepEqual(ext.templateChoices(versions, "1.1.0", "review", "guild").map((c) => c.reference), ["guild@1.0.0/review"]);
  } finally {
    globalThis.fetch = previousFetch;
    globalThis.window = previousWindow;
    await server.close();
  }
});
