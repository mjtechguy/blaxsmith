import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { createServer } from "vite";

let server;
let m;
before(async () => {
  server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  m = await server.ssrLoadModule("/src/sign-in.ts");
});
after(() => server.close());

const run = (events, from) => events.reduce((state, event) => m.signInReducer(state, event), from ?? m.initialSignIn);
const device = { verificationUrl: "https://auth.openai.com/codex/device", userCode: "ABCD-1234", expiresAt: "2026-09-24T12:15:00Z" };

test("device code: start, wait with the code, succeed", () => {
  const waiting = run([{ type: "start" }, { type: "started", device }]);
  assert.equal(waiting.phase, "waiting");
  assert.deepEqual(waiting.device, device);
  assert.equal(waiting.attempt, 1);
  const done = run([{ type: "verify" }, { type: "succeed" }], waiting);
  assert.equal(done.phase, "succeeded");
});

test("API key validation goes straight from starting to verifying", () => {
  assert.equal(run([{ type: "start" }, { type: "verify" }]).phase, "verifying");
  const failed = run([{ type: "start" }, { type: "verify" }, { type: "fail", error: "the provider rejected this credential" }]);
  assert.equal(failed.phase, "failed");
  assert.equal(failed.error, "the provider rejected this credential");
});

test("cancel stops the flow and a late result cannot revive it", () => {
  const cancelled = run([{ type: "start" }, { type: "started", device }, { type: "cancel" }]);
  assert.equal(cancelled.phase, "cancelled");
  assert.equal(run([{ type: "succeed" }], cancelled).phase, "cancelled");
  assert.equal(run([{ type: "fail", error: "late" }], cancelled).phase, "cancelled");
});

test("expiry only applies while waiting, and retry starts a new attempt", () => {
  const expired = run([{ type: "start" }, { type: "started", device }, { type: "expire" }]);
  assert.equal(expired.phase, "failed");
  assert.match(expired.error, /expired/);
  assert.equal(run([{ type: "start" }, { type: "expire" }]).phase, "starting");
  const again = run([{ type: "retry" }, { type: "start" }], expired);
  assert.equal(again.phase, "starting");
  assert.equal(again.attempt, 2);
  assert.equal(again.device, undefined);
  assert.equal(again.error, undefined);
});

test("invalid transitions are ignored", () => {
  assert.equal(run([{ type: "succeed" }]).phase, "idle");
  assert.equal(run([{ type: "retry" }]).phase, "idle");
  assert.equal(run([{ type: "start" }, { type: "start" }]).attempt, 1);
  const done = run([{ type: "start" }, { type: "succeed" }]);
  assert.equal(run([{ type: "cancel" }, { type: "retry" }], done).phase, "succeeded");
});

test("countdown helpers", () => {
  assert.equal(m.secondsLeft("2026-09-24T12:15:00Z", Date.parse("2026-09-24T12:13:30.200Z")), 90);
  assert.equal(m.secondsLeft("2026-09-24T12:15:00Z", Date.parse("2026-09-24T12:20:00Z")), 0);
  assert.equal(m.secondsLeft("not a time"), 0);
  assert.equal(m.formatCountdown(90), "1:30");
  assert.equal(m.formatCountdown(5), "0:05");
});
