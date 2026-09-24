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
    const { adminSectionFor } = await server.ssrLoadModule("/src/admin.ts");
    const source = { repositoryUrl: "https://github.com/example/guild", gitRef: "main", manifestPath: "", overlayManifestJson: "{}" };
    await ext.listExtensions();
    await ext.previewExtensionInstall(source);
    await ext.installExtension(source, "a".repeat(40), ["mcp:foundry"]);
    await ext.checkExtensionUpdate("e1");
    await ext.grantExtension("e1", "p1", "project", "");
    await ext.revokeExtensionGrant("g1");
    const C = "C".repeat(43);
    assert.deepEqual(requests.map((r) => [r.url, r.csrf]), [
      ["blaxsmith.api.v1.ExtensionService/ListExtensions", null],
      ["blaxsmith.api.v1.ExtensionService/PreviewExtensionInstall", C],
      ["blaxsmith.api.v1.ExtensionService/InstallExtension", C],
      ["blaxsmith.api.v1.ExtensionService/CheckExtensionUpdate", C],
      ["blaxsmith.api.v1.ExtensionService/GrantExtension", C],
      ["blaxsmith.api.v1.ExtensionService/RevokeExtensionGrant", C],
    ]);
    assert.equal(requests[2].body.expectedCommit, "a".repeat(40));
    assert.deepEqual(requests[2].body.approvedPermissions, ["mcp:foundry"]);
    assert.equal(requests[2].body.source.overlayManifestJson, "{}");
    assert.deepEqual(requests[4].body, { extensionId: "e1", projectId: "p1", granteeKind: "project" });

    const permissions = [{ id: "mcp:foundry", optional: false }, { id: "mcp:serena", optional: true }, { id: "hook:foundry-serena", optional: true }];
    assert.deepEqual(ext.approvedPermissions(permissions, new Set()), ["mcp:foundry"]);
    assert.deepEqual(ext.approvedPermissions(permissions, new Set(["mcp:serena", "undeclared"])), ["mcp:foundry", "mcp:serena"]);
    assert.deepEqual(["/admin/extensions", "/admin/extensions/new", "/admin/extensions/e1"].map(adminSectionFor),
      ["/admin/extensions", "/admin/extensions", "/admin/extensions"]);
  } finally {
    globalThis.fetch = previousFetch;
    globalThis.window = previousWindow;
    await server.close();
  }
});
