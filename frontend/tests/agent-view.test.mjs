import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { createServer } from "vite";

let server;
let view;
before(async () => {
  server = await createServer({ server: { middlewareMode: true, hmr: false }, appType: "custom" });
  view = await server.ssrLoadModule("/src/agent-view.ts");
});
after(() => server.close());

let seq = 0;
const ev = (p, attemptId = "att-1") => ({ id: String(++seq), attemptId, at: "2026-09-24T10:00:00Z", kind: "attempt.progress", type: p.type, p });

test("work log folds consecutive tool calls into one group and replaces items by id", () => {
  const { entries, plan } = view.workLog([
    ev({ type: "assistant.message", id: "m1", text: "Looking around." }),
    ev({ type: "tool.started", id: "t1", tool: "Read", input: "a.go", status: "running" }),
    ev({ type: "tool.completed", id: "t2", tool: "Grep", input: "created_at", status: "failed", duration_ms: 12 }),
    ev({ type: "tool.completed", id: "t1", tool: "Read", input: "a.go", status: "completed", duration_ms: 40 }),
    ev({ type: "plan.updated", id: "plan", items: [{ text: "Run tests", status: "completed" }, { text: "Fix header", status: "in_progress" }, { text: "", status: "pending" }] }),
    ev({ type: "command", id: "c1", cmd: "go test ./...", status: "running" }),
    ev({ type: "tool.completed", id: "t3", tool: "Glob", input: "**/*.go", status: "completed" }),
    ev({ type: "command", id: "c1", cmd: "go test ./...", status: "failed", exit: 1, duration_ms: 2500 }),
    ev({ type: "file.changed", id: "f1", path: "a.go", status: "completed", added: 3, removed: 1 }),
    ev({ type: "phase", name: "verify" }),
    { ...ev({ type: "progress" }), kind: "attempt.started" },
  ]);
  assert.deepEqual(entries.map((entry) => entry.kind), ["message", "tools", "command", "tools", "file", "note"]);
  const group = entries[1];
  assert.deepEqual(group.items.map((item) => [item.id, item.status, item.durationMs]), [["t1", "completed", 40], ["t2", "failed", 12]]);
  assert.equal(view.groupSummary(group.items), "2 tool calls · 1 failed");
  assert.equal(entries[2].exit, 1);
  assert.equal(entries[2].status, "failed");
  assert.equal(entries[3].items.length, 1);
  assert.deepEqual(plan, [{ text: "Run tests", status: "completed" }, { text: "Fix header", status: "in_progress" }]);
  assert.deepEqual(view.planProgress(plan), { done: 1, total: 2, current: "Fix header" });
});

test("work log keeps items from different attempts apart", () => {
  const { entries } = view.workLog([
    ev({ type: "command", id: "item_1", cmd: "make", status: "failed", exit: 2 }, "att-1"),
    ev({ type: "command", id: "item_1", cmd: "make", status: "completed", exit: 0 }, "att-2"),
  ]);
  assert.deepEqual(entries.map((entry) => entry.exit), [2, 0]);
});

const task = (key, state, fields = {}) => ({ key, state, kind: "implement", dependsOn: [], activeAttemptId: "", ...fields });

test("status rollup prefers approval over input over work over plan ready", () => {
  const tasks = [
    task("plan", "succeeded", { kind: "plan" }),
    task("implement", "pending", { dependsOn: ["plan"] }),
    task("verify", "running"),
    task("review", "running"),
    task("gate", "running"),
    task("old", "blocked"),
  ];
  const open = [
    { stage: "review", kind: "question", state: "open" },
    { stage: "gate", kind: "approval", state: "open" },
    { stage: "verify", kind: "question", state: "answered" },
  ];
  const statuses = tasks.map((t) => view.stageStatus(t, open, tasks));
  assert.deepEqual(statuses, ["plan_ready", null, "working", "awaiting_input", "needs_approval", "failed"]);
  assert.equal(view.rollup(statuses), "needs_approval");
  assert.equal(view.rollup(["done", "failed", null]), "failed");
  assert.equal(view.rollup(["done", "plan_ready", "working"]), "working");
  assert.equal(view.rollup([null, undefined]), null);
  assert.equal(view.runStatus("active", tasks, open), "needs_approval");
  assert.equal(view.runStatus("succeeded", [task("a", "succeeded")], []), "done");
  // No stage signal: the run state is the pill, so queued and halted runs never show "—".
  assert.equal(view.runStatus("queued", [task("a", "pending")], []), "queued");
  assert.equal(view.runStatus("cancel_requested", [task("a", "pending")], []), "cancel_requested");
  assert.equal(view.runStatus("cancelled", [task("a", "cancelled")], []), "cancelled");
  assert.equal(view.runStatus("cancelled", [task("a", "succeeded")], []), "cancelled");
  // Stage signals still outrank the fallback, which ranks below done.
  assert.equal(view.runStatus("cancelled", [task("a", "blocked")], []), "failed");
  assert.equal(view.runStatus("cancel_requested", [task("a", "running")], []), "working");
  assert.deepEqual(view.statusOrder, ["needs_approval", "awaiting_input", "working", "plan_ready", "failed", "done", "cancel_requested", "cancelled", "queued"]);
  assert.equal(view.rollup(["queued", "done"]), "done");
  for (const status of view.statusOrder) assert.ok(view.statusLabels[status], status);
  // A plan stage whose dependents started is just done; a presented review needs approval.
  const started = [task("plan", "succeeded", { kind: "plan" }), task("implement", "running", { dependsOn: ["plan"] }), task("human-review", "pending", { kind: "human_review" })];
  assert.equal(view.stageStatus(started[0], [], started), "done");
  assert.equal(view.stageStatus(started[2], [], started, true), "needs_approval");
  assert.equal(view.needsYou("awaiting_input"), true);
  assert.equal(view.needsYou("working"), false);
  assert.equal(view.attentionTitle(2), "(2) Blaxsmith");
  assert.equal(view.attentionTitle(0), "Blaxsmith");
  assert.equal(view.attentionTitle(120), "(99+) Blaxsmith");
});

test("question card keys: digits select, Enter sends, Escape cancels text", () => {
  assert.deepEqual(view.cardKey("1", 3, false), { action: "select", index: 0 });
  assert.deepEqual(view.cardKey("3", 3, false), { action: "select", index: 2 });
  assert.equal(view.cardKey("4", 3, false), null);
  assert.equal(view.cardKey("0", 3, false), null);
  assert.equal(view.cardKey("2", 3, true), null, "digits typed into the reply stay text");
  assert.deepEqual(view.cardKey("Enter", 3, false), { action: "submit" });
  assert.deepEqual(view.cardKey("Enter", 0, true), { action: "submit" });
  assert.equal(view.cardKey("Enter", 3, true, { shift: true }), null, "Shift+Enter is a newline");
  assert.equal(view.cardKey("Enter", 3, false, { composing: true }), null, "IME composition is left alone");
  assert.deepEqual(view.cardKey("Escape", 3, true), { action: "cancel_text" });
  assert.equal(view.cardKey("Escape", 3, false), null);
  assert.equal(view.cardKey("1", 3, false, { meta: true }), null, "browser shortcuts pass through");
});

test("approval buttons map onto the interaction's own options", () => {
  const options = [{ id: "go", label: "Go", recommended: true }, { id: "revise", label: "Revise" }, { id: "cancel", label: "Cancel" }];
  const { approve, decline } = view.approvalChoices(options);
  assert.equal(approve.id, "go");
  assert.equal(decline.id, "cancel");
  assert.equal(view.approvalChoices([{ id: "a", label: "Ship it", recommended: true }]).approve.id, "a");
  assert.equal(view.approvalChoices([{ id: "a", label: "Ship it" }]).approve, undefined);
});

test("factory layers show fan-out and fan-in independently of API order and stop on unresolved dependencies", () => {
  const tasks = [
    { key: "accept", dependsOn: ["review", "verify"] },
    { key: "verify", dependsOn: ["implement"] },
    { key: "implement", dependsOn: [] },
    { key: "review", dependsOn: ["implement"] },
  ];
  const graph = view.stageLayers(tasks);
  assert.deepEqual(graph.layers.map((layer) => layer.map((t) => t.key)), [["implement"], ["verify", "review"], ["accept"]]);
  assert.deepEqual(graph.unresolved, []);
  assert.equal(tasks[0].key, "accept");
  const broken = [{ key: "missing", dependsOn: ["absent"] }, { key: "a", dependsOn: ["b"] }, { key: "b", dependsOn: ["a"] }];
  assert.deepEqual(view.stageLayers(broken), { layers: [], unresolved: broken });
  assert.deepEqual(view.stageLayers([]), { layers: [], unresolved: [] });
});

test("native acceptance needs approval even without a human-review stage", () => {
  const tasks = [{ key: "implement", kind: "implement", state: "succeeded", dependsOn: [] }, { key: "verify", kind: "verify", state: "succeeded", dependsOn: ["implement"] }];
  assert.equal(view.runStatus("succeeded", tasks, [], true), "needs_approval");
  assert.equal(view.runStatus("succeeded", tasks, [], false), "done");
  assert.notEqual(view.runStatus("active", tasks, [], true), "needs_approval");
});

test("factory map links stages and acceptance without presenting an old package as current acceptance", async (t) => {
  const previousWindow = globalThis.window;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  t.after(() => { globalThis.window = previousWindow; });
  const { renderFactoryMap } = await server.ssrLoadModule("/tests/render-app.tsx");
  const pending = await renderFactoryMap(undefined);
  assert.match(pending, /Awaiting evidence package/);
  assert.match(pending, /aria-current="step"/);
  assert.match(pending, /tab=stages.*stage=verify/);
  assert.match(pending, /1.*\/.*2.*repairs requested/s);
  assert.match(pending, /Platform checks · no model/);
  assert.match(pending, /tab=review/);
  const manual = { acceptanceMode: "manual", integratedCommit: "a".repeat(40) };
  assert.match(await renderFactoryMap(manual), /Awaiting human decision/);
  const policy = { ...manual, acceptanceMode: "policy" };
  assert.match(await renderFactoryMap(policy), /Accepted by policy/);
  assert.match(await renderFactoryMap({ ...manual, decision: { action: "approve" } }), /Approved by a person/);
  assert.match(await renderFactoryMap({ ...manual, decision: { action: "request_changes" } }), /Changes requested/);
  const stale = await renderFactoryMap(policy, "active");
  assert.doesNotMatch(stale, /Accepted by policy|Candidate a/);
  assert.match(stale, /After execution and checks/);
  const unavailable = await renderFactoryMap(policy, "succeeded", true);
  assert.match(unavailable, /Acceptance unavailable/);
  assert.doesNotMatch(unavailable, /Accepted by policy/);
});
