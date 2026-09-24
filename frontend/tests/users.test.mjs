import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("user admin mutations send CSRF, reads do not, and role gating mirrors the server", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "C".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    requests.push({ method: String(input).split("UserAdminService/")[1], body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") });
    return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const users = await server.ssrLoadModule("/src/users.ts");
    const { isAccountLinkRoute } = await server.ssrLoadModule("/src/auth.ts");
    const { ConnectError, Code } = await server.ssrLoadModule("@connectrpc/connect");
    await users.listMembers();
    await users.inviteUser("jordan@example.com", "Jordan", "member");
    await users.setUserEmail("p-1", "new@example.com");
    await users.setUserRole("p-1", "viewer");
    await users.setUserEnabled("p-1", false);
    await users.issueResetLink("p-1");
    await users.revokeUserSessions("p-1");
    await users.getAccountLink("tok");
    await users.completeAccountLink("tok", "long enough password");
    await users.completeAccountLink("tok", "long enough password", "Jordan Lee", "jordan@example.com");
    const csrf = "C".repeat(43);
    assert.deepEqual(requests.map((r) => [r.method, r.csrf, r.body]), [
      ["ListOrgMembers", null, {}],
      ["InviteUser", csrf, { email: "jordan@example.com", displayName: "Jordan", role: "member" }],
      ["SetUserEmail", csrf, { principalId: "p-1", email: "new@example.com" }],
      ["SetUserRole", csrf, { principalId: "p-1", role: "viewer" }],
      ["SetUserEnabled", csrf, { principalId: "p-1" }],
      ["IssueResetLink", csrf, { principalId: "p-1" }],
      ["RevokeUserSessions", csrf, { principalId: "p-1" }],
      ["GetAccountLink", null, { token: "tok" }],
      ["CompleteAccountLink", csrf, { token: "tok", password: "long enough password" }],
      ["CompleteAccountLink", csrf, { token: "tok", password: "long enough password", displayName: "Jordan Lee", email: "jordan@example.com" }],
    ]);
    assert.deepEqual(users.assignableRoles({ role: "owner" }), ["owner", "admin", "member", "viewer"]);
    assert.deepEqual(users.assignableRoles({ role: "admin" }), ["admin", "member", "viewer"]);
    assert.deepEqual(users.assignableRoles({ role: "member" }), []);
    assert.equal(users.canManage({ role: "admin" }, "owner"), false);
    assert.equal(users.canManage({ role: "admin" }, "admin"), true);
    assert.equal(users.canManage({ role: "owner" }, "owner"), true);
    assert.equal(users.setupUrl("a_b-c"), "https://blaxsmith.test/setup/a_b-c");
    assert.equal(isAccountLinkRoute("/setup/a_b-c"), true);
    assert.equal(isAccountLinkRoute("/setup/"), false);
    assert.equal(isAccountLinkRoute("/setup/a/b"), false);
    assert.match(users.userAdminError(new ConnectError("the organization must keep an active owner", Code.FailedPrecondition)), /at least one active owner/);
    assert.match(users.userAdminError(new ConnectError("only an owner can grant or change owner access", Code.PermissionDenied)), /Only an owner/);
    assert.match(users.userAdminError(new ConnectError("you cannot disable your own account", Code.FailedPrecondition)), /your own account/);
    assert.match(users.userAdminError(new ConnectError("that email is already in use", Code.AlreadyExists)), /email is already used/);
    assert.match(users.userAdminError(new ConnectError("enter a valid email address", Code.InvalidArgument)), /valid email/);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});
