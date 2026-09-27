import assert from "node:assert/strict";
import { test } from "node:test";
import { QueryClient } from "@tanstack/react-query";
import { createServer } from "vite";

test("review correction sends trimmed feedback with CSRF", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  let request;
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "A".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    request = { url: String(input), body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") };
    return new Response(JSON.stringify({ decision: { id: "decision" } }), { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const { decideReview } = await server.ssrLoadModule("/src/workflow.ts");
    await decideReview("run", "package", "request_changes", "  Fix the failing check.  ");
    assert.equal(request.url, "https://blaxsmith.test/api/blaxsmith.api.v1.WorkflowService/DecideReview");
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.feedback, "Fix the failing check.");
    assert.equal(request.body.action, "request_changes");
    assert.equal(request.body.packageId, "package");
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});

test("model access revocation sends the selected grant with CSRF", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  let request;
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "A".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    request = { url: String(input), body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") };
    return new Response(JSON.stringify({ accessId: "grant" }), { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const { revokeProjectModelAccess } = await server.ssrLoadModule("/src/workflow.ts");
    await revokeProjectModelAccess("grant");
    assert.equal(request.url, "https://blaxsmith.test/api/blaxsmith.api.v1.WorkflowService/RevokeProjectModelAccess");
    assert.equal(request.csrf, "A".repeat(43));
    assert.deepEqual(request.body, { accessId: "grant" });
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});

test("live activity merges replay once and detects missing events", async () => {
  const previousWindow = globalThis.window;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { appendRunEvent, liveEventsUrl, parseLiveEvent, recoverRunEventBatch } = await server.ssrLoadModule("/src/workflow.ts");
    const parse = (id, runId = "run") => parseLiveEvent(JSON.stringify({ id: String(id), runId, kind: "attempt.started", occurredAt: "2026-09-23T00:00:00Z" }), "run");
    assert.equal(liveEventsUrl("run", 12n), "https://blaxsmith.test/api/runs/run/events?after=12");
    assert.throws(() => parse(1, "other"), /Invalid run event/);
    let data = { pages: [{ events: [], nextAfterId: 0n }], pageParams: [0n] };
    for (let id = 1; id <= 101; id++) {
      const merged = appendRunEvent(data, parse(id));
      assert.equal(merged.gap, false);
      data = merged.data;
    }
    assert.equal(data.pages.length, 2);
    assert.equal(data.pages[1].nextAfterId, 101n);
    assert.deepEqual(data.pageParams, [0n, 100n]);
    assert.equal(appendRunEvent(data, parse(101)).data, data);
    assert.equal(appendRunEvent(data, parse(103)).gap, true);
    assert.equal(data.pages[1].events.length, 1);
    data = { pages: [{ events: [], nextAfterId: 0n }], pageParams: [0n] };
    const cursor = () => data.pages.at(-1).nextAfterId;
    const fetchPage = async (after) => {
      const end = Math.min(Number(after) + 100, 600);
      return { events: Array.from({ length: end - Number(after) }, (_, index) => parse(Number(after) + index + 1)), nextAfterId: BigInt(end) };
    };
    const append = (event) => { data = appendRunEvent(data, event).data; };
    assert.equal(await recoverRunEventBatch(fetchPage, cursor, append), true);
    assert.equal(cursor(), 500n);
    assert.equal(await recoverRunEventBatch(fetchPage, cursor, append), false);
    assert.equal(cursor(), 600n);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
  }
});

test("session scope change removes cached run activity", async () => {
  const previousWindow = globalThis.window;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { clearWorkspaceCache, sessionQueryKey } = await server.ssrLoadModule("/src/auth.ts");
    const queryClient = new QueryClient();
    queryClient.setQueryData(sessionQueryKey, { organizationId: "first", principalId: "user" });
    queryClient.setQueryData(["run-events", "first:user", "run"], { pages: [{ events: [{ id: 1n }], nextAfterId: 1n }], pageParams: [0n] });
    await clearWorkspaceCache(queryClient);
    assert.equal(queryClient.getQueryData(["run-events", "first:user", "run"]), undefined);
    assert.equal(queryClient.getQueryData(sessionQueryKey).organizationId, "first");
    queryClient.setQueryData(["admin-overview", "first"], { liveAttempts: [] });
    await clearWorkspaceCache(queryClient);
    assert.equal(queryClient.getQueryData(["admin-overview", "first"]), undefined);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
  }
});

test("goal writes send CSRF, retry keys, and exact revisions; reads carry cursors", async () => {
  const previousWindow = globalThis.window, previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true, hmr: false }, appType: "custom" });
  let request;
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) return new Response(JSON.stringify({ token: "A".repeat(43) }), { headers: { "content-type": "application/json" } });
    request = { body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") };
    return new Response("{}", { headers: { "content-type": "application/json" } });
  };
  try {
    const goals = await server.ssrLoadModule("/src/goals.ts");
    await goals.createGoal("project", "retry-create", "Title", "Brief");
    assert.equal(request.csrf, "A".repeat(43));
    assert.deepEqual(request.body, { projectId: "project", requestKey: "retry-create", title: "Title", brief: "Brief" });
    await goals.replyGoal({ goalId: "goal", requestKey: "retry-answer", expectedRevision: 7n, kind: "answer", questionId: "depth", optionIds: ["focused"], text: "Small scope" });
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.expectedRevision, "7");
    assert.equal(request.body.requestKey, "retry-answer");
    await goals.getGoal("goal", 40n);
    assert.equal(request.csrf, null);
    assert.deepEqual(request.body, { goalId: "goal", beforeSequence: "40" });
    await goals.startGoalPlanning({ goalId: "goal", expectedRevision: 7n, requestKey: "plan-retry", harness: "codex", model: "test-model", effort: "medium", scope: ".", runtimeSeconds: 900, instructionFiles: ["docs/stack.md"], skillFiles: ["skills/testing/SKILL.md"] });
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.expectedRevision, "7");
    assert.equal(request.body.runtimeSeconds, 900);
    assert.deepEqual(request.body.instructionFiles, ["docs/stack.md"]);
    assert.deepEqual(request.body.skillFiles, ["skills/testing/SKILL.md"]);
    await goals.saveGoalPlan({ goalId: "goal", requestKey: "save-retry", expectedGoalRevision: 7n, expectedPlanVersion: 2n, evidenceId: "artifact" });
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.expectedPlanVersion, "2");
    assert.equal(request.body.evidenceId, "artifact");
    await goals.getGoalPlans("goal", 20n);
    assert.equal(request.csrf, null);
    assert.deepEqual(request.body, { goalId: "goal", beforeVersion: "20" });
    const execution = { goalId: "goal", expectedGoalRevision: 7n, planVersion: 2n, harness: "codex", model: "test-model", effort: "medium", runtimeSeconds: 900, correctionCycles: 2, acceptance: "policy", instructionFiles: ["docs/architecture.md"], skillFiles: ["skills/testing/SKILL.md", "skills/testing/checks.md"] };
    await goals.previewGoalExecution("project", ".", execution);
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.goalExecution.planVersion, "2");
    assert.equal(request.body.goalExecution.acceptance, "policy");
    assert.deepEqual(request.body.goalExecution.instructionFiles, execution.instructionFiles);
    assert.deepEqual(request.body.goalExecution.skillFiles, execution.skillFiles);
    await goals.launchGoalExecution("project", "execution-retry", ".", execution, { bundleSha256: "a".repeat(64), verificationSha256: "b".repeat(64) });
    assert.equal(request.csrf, "A".repeat(43));
    assert.equal(request.body.launchKey, "execution-retry");
    assert.equal(request.body.expectedBundleSha256, "a".repeat(64));
    assert.equal(request.body.expectedVerificationSha256, "b".repeat(64));
    assert.equal(request.body.goalExecution.expectedGoalRevision, "7");
    assert.deepEqual(request.body.goalExecution.instructionFiles, execution.instructionFiles);
    assert.deepEqual(request.body.goalExecution.skillFiles, execution.skillFiles);
    await goals.listGoals("project", "older");
    assert.deepEqual(request.body, { projectId: "project", beforeId: "older" });
  } finally {
    await server.close(); globalThis.window = previousWindow; globalThis.fetch = previousFetch;
  }
});

test("visual plan edits preserve IDs, citations, multiline criteria and unrelated assignments", async () => {
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { PlanEditor } = await server.ssrLoadModule("/src/plan-editor.tsx");
    const task = (id) => ({ id, title: id, phase: "P1", reason: "Preserve behavior", instructions: "Inspect callers", depends_on: null, requirement_ids: ["R1"], acceptance: ["First line\nSecond line"], validation: null });
    const original = { schema_version: "anvil.plan/v1alpha1", title: "Saved search", summary: "Retain user settings", assumptions: null, out_of_scope: [], open_questions: [], phases: [{ id: "P1", title: "Deliver", outcome: "Working search" }], requirements: [{ id: "R1", description: "Reload restores settings", sources: ["brief", "message:2"], examples: ["Reload the page"] }], tasks: [task("T1"), task("T2")] };
    let value = structuredClone(original);
    function* nodes(node) {
      if (!node || typeof node !== "object") return;
      if (Array.isArray(node)) { for (const child of node) yield* nodes(child); return; }
      if (typeof node.type === "function") { yield* nodes(node.type(node.props)); return; }
      yield node;
      yield* nodes(node.props?.children);
    }
    const text = (node) => Array.isArray(node) ? node.map(text).join("") : typeof node === "object" ? text(node?.props?.children ?? "") : String(node ?? "");
    const tree = () => [...nodes(PlanEditor({ value, onChange: (next) => { value = next; } }))];
    const field = (label) => {
      const parent = tree().find((n) => n.type === "label" && text(n.props.children?.[0]) === label);
      assert.ok(parent, `labelled field ${label}`);
      return parent.props.children[1];
    };
    field("T1 instructions").props.onChange({ target: { value: "Read callers.\nPreserve errors." } });
    field("T1 acceptance criteria 1").props.onChange({ target: { value: "Reload succeeds\nInvalid input keeps prior settings" } });
    tree().find((n) => n.type === "button" && text(n) === "Add t1 acceptance criteria item").props.onClick();
    field("T1 acceptance criteria 2").props.onChange({ target: { value: "Keyboard flow works" } });
    tree().find((n) => n.type === "input" && n.props.type === "checkbox").props.onChange({ target: { checked: true } });
    field("R1 examples 1").props.onChange({ target: { value: "Save filters, reload, restore" } });
    const saved = JSON.parse(JSON.stringify(value));
    assert.equal(saved.tasks[0].instructions, "Read callers.\nPreserve errors.");
    assert.deepEqual(saved.tasks[0].acceptance, ["Reload succeeds\nInvalid input keeps prior settings", "Keyboard flow works"]);
    assert.deepEqual(saved.tasks[0].depends_on, ["T2"]);
    assert.deepEqual(saved.tasks[1], original.tasks[1]);
    assert.deepEqual(saved.requirements[0].sources, original.requirements[0].sources);
    assert.deepEqual(saved.phases, original.phases);
    assert.deepEqual(saved.requirements[0].examples, ["Save filters, reload, restore"]);
    assert.equal(original.tasks[0].instructions, "Inspect callers");
    assert.equal(saved.assumptions, null);
  } finally { await server.close(); }
});
