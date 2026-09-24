import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

const ids = { organizationId: "00000000-0000-4000-8000-000000000001", principalId: "00000000-0000-4000-8000-000000000002" };
const session = (extra = {}) => ({ ...ids, role: "member", accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString(), ...extra });
const scope = `${ids.organizationId}:${ids.principalId}`;
const profile = { principalId: ids.principalId, email: "jordan@example.com", emailVerified: false, displayName: "Jordan Lee", handle: "jordan",
  organizationId: ids.organizationId, organizationSlug: "acme", organizationName: "Acme Engineering", role: "member", createdAt: new Date().toISOString(), emailRequired: false };

async function withApp(run) {
  const saved = { window: globalThis.window, fetch: globalThis.fetch, localStorage: globalThis.localStorage, matchMedia: globalThis.matchMedia };
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  globalThis.localStorage = { getItem: () => null, setItem: () => {} };
  globalThis.matchMedia = () => ({ matches: false, addEventListener() {}, removeEventListener() {} });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    const method = String(input).split("/").at(-1);
    if (method === "GetCsrf") return new Response(JSON.stringify({ token: "C".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    requests.push({ method, body: init?.body ? JSON.parse(new TextDecoder().decode(init.body)) : {}, csrf: new Headers(init?.headers).get("X-Blaxsmith-CSRF") });
    return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    await run({ app: await server.ssrLoadModule("/tests/render-app.tsx"), load: (path) => server.ssrLoadModule(path), requests });
  } finally {
    await server.close();
    Object.assign(globalThis, saved);
  }
}

test("the login page asks for an email, not a username", async () => {
  await withApp(async ({ app }) => {
    const html = await app.renderLogin();
    assert.match(html, /<label[^>]*>Email<\/label>/);
    assert.match(html, /type="email"[^>]*autoComplete="username"|autoComplete="username"[^>]*type="email"|type="email"/);
    assert.doesNotMatch(html, /<label[^>]*>Username<\/label>/);
    // The organization is asked for only when the server says the account has several.
    assert.doesNotMatch(html, /<label[^>]*>Organization<\/label>/);
    assert.match(html, /Enter your old username once/);
    assert.match(html, /class="auth-form" novalidate=""/i, "a legacy username is not blocked by browser email validation");
  });
});

test("an email_required session gets the routed set-email page and nothing else", async () => {
  await withApp(async ({ app }) => {
    const required = session({ emailRequired: true });
    const page = await app.renderApp("/me/email", required);
    assert.match(page, /Set your email/);
    assert.match(page, /<label[^>]*>Email<\/label>/);
    assert.match(page, /<label[^>]*>Current password<\/label>/);
    assert.doesNotMatch(page, /<dialog/, "a routed page, not a modal");
    assert.doesNotMatch(page, /aria-haspopup="menu"/, "no workspace shell while the email is missing");
    for (const path of ["/", "/admin/users", "/me/settings/profile"]) {
      const html = await app.renderApp(path, required);
      assert.doesNotMatch(html, /aria-haspopup="menu"|Page not found/, `${path} is not rendered for an email_required session`);
    }
    // Once an email is set the page is not reachable and the shell returns.
    assert.doesNotMatch(await app.renderApp("/me/email", session()), /Set your email/);
    assert.match(await app.renderApp("/", session()), /aria-haspopup="menu"/);
  });
});

test("account settings forms: profile, password, sessions", async () => {
  await withApp(async ({ app }) => {
    const profileKey = ["my-profile", scope];
    const profilePage = await app.renderApp("/me/settings/profile", session(), [[profileKey, profile]]);
    assert.match(profilePage, /value="Jordan Lee"/);
    assert.match(profilePage, /type="email"[^>]*value="jordan@example.com"|value="jordan@example.com"/);
    assert.match(profilePage, /Not verified yet/);
    assert.match(profilePage, /class="save-bar"/, "profile edits use the shared save bar");
    assert.doesNotMatch(profilePage, /<label[^>]*>Current password<\/label>/, "the password is asked for only once the email changes");
    assert.doesNotMatch(profilePage, /Coming soon/);

    const password = await app.renderApp("/me/settings/password", session(), [[profileKey, profile]]);
    for (const label of ["Current password", "New password", "Confirm new password"]) assert.match(password, new RegExp(`<label[^>]*>${label}</label>`));
    assert.equal([...password.matchAll(/type="password"/g)].length, 3);
    assert.match(password, /Change password/);
    assert.match(password, /value="jordan@example.com"/, "password managers see the account email");

    const now = Date.now();
    const at = (minutes) => new Date(now - minutes * 60_000).toISOString();
    const sessions = [
      { id: "s1", current: true, organizationSlug: "acme", createdAt: at(10), lastSeenAt: at(1), expiresAt: at(-6000), sourceAddress: "127.0.0.1", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) Version/18.0 Safari/605.1.15" },
      { id: "s2", current: false, organizationSlug: "acme", createdAt: at(900), lastSeenAt: "", expiresAt: at(-5000), sourceAddress: "203.0.113.9", userAgent: "Mozilla/5.0 (Windows NT 10.0) Chrome/140.0 Safari/537.36" },
    ];
    const list = await app.renderApp("/me/settings/sessions", session(), [[profileKey, profile], [["my-sessions", scope], sessions]]);
    assert.match(list, /Safari on macOS/);
    assert.match(list, /Chrome on Windows/);
    assert.match(list, /This session/);
    assert.match(list, /Sign out all others/);
    assert.equal([...list.matchAll(/>\s*(<svg[^>]*>.*?<\/svg>)?\s*Sign out<\/button>/gs)].length, 1, "only other sessions have a Sign out button");
  });
});

test("the account client sends CSRF on mutations only and maps errors", async () => {
  await withApp(async ({ load, requests }) => {
    const account = await load("/src/account.ts");
    const { ConnectError, Code } = await load("@connectrpc/connect");
    const { loginError } = await load("/src/auth.ts");
    await account.getMyProfile();
    await account.updateMyProfile({ displayName: "Jordan" });
    await account.updateMyProfile({ email: "j@example.com", currentPassword: "correct horse battery" });
    await account.changeMyPassword("old password 12", "new password 12");
    await account.listMySessions();
    await account.revokeMySession("s2");
    await account.revokeMyOtherSessions();
    const csrf = "C".repeat(43);
    assert.deepEqual(requests.map((r) => [r.method, r.csrf, r.body]), [
      ["GetMyProfile", null, {}],
      ["UpdateMyProfile", csrf, { displayName: "Jordan" }],
      ["UpdateMyProfile", csrf, { email: "j@example.com", currentPassword: "correct horse battery" }],
      ["ChangeMyPassword", csrf, { currentPassword: "old password 12", newPassword: "new password 12" }],
      ["ListMySessions", null, {}],
      ["RevokeMySession", csrf, { sessionId: "s2" }],
      ["RevokeMyOtherSessions", csrf, {}],
    ]);
    assert.match(account.accountError(new ConnectError("current password is incorrect", Code.PermissionDenied)), /current password is not correct/);
    assert.match(account.accountError(new ConnectError("that email is already in use", Code.AlreadyExists)), /already used/);
    assert.match(account.accountError(new ConnectError("password must be valid UTF-8 and 12–1024 bytes", Code.InvalidArgument)), /12–1024/);
    assert.match(account.accountError(new ConnectError("too many attempts", Code.ResourceExhausted)), /Too many attempts/);
    assert.equal(account.emailProblem("jordan@example.com"), undefined);
    for (const bad of ["jordan", "jordan@example", "a b@example.com", ""]) assert.ok(account.emailProblem(bad), bad);
    assert.equal(account.passwordProblem("short"), "Use at least 12 characters.");
    assert.equal(account.passwordProblem("twelve chars"), undefined);
    assert.equal(account.personLabel({ displayName: "", email: "j@example.com", handle: "j" }), "j@example.com");
    assert.equal(account.initials({ displayName: "", email: "jordan.lee@example.com" }), "JL");
    assert.equal(account.browserLabel("Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0"), "Firefox on Linux");
    assert.equal(account.browserLabel(""), "Unknown browser");
    assert.match(loginError(new ConnectError("authentication required", Code.Unauthenticated)), /email and password were not accepted/);
    assert.match(loginError(new ConnectError("choose the organization to sign in to", Code.FailedPrecondition)), /more than one organization/);
  });
});
