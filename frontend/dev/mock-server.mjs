// Dev-only mock of the Blaxsmith API for exercising the run page (not bundled).
//   npm run dev:mock   → mock API on :8001 + Vite on :3000, then open the printed URL.
// Serves just enough Connect JSON, the run SSE stream, and the terminal WebSocket from
// docs/interactive-sessions.md. The scenario: a Forge-style interview and an approval gate on
// `plan`, plus a 3-cycle verify loop that hits its cap and escalates. Answering the
// escalation moves the run to human review.
import { readFileSync } from "node:fs";
import { createServer as createHttp } from "node:http";
import { WebSocketServer } from "ws";
import { createServer as createVite } from "vite";

const now = () => new Date().toISOString();
const principalId = "p-you";
const runId = "run-guild";
const projectId = "proj-demo";
const task = (key, harness, model, effort, dependsOn, state = "pending", activeAttemptId = "") =>
  ({ id: `t-${key}`, key, state, generation: activeAttemptId ? "1" : "0", maxAttempts: 3, activeAttemptId, dependsOn, harness, model, effort, instructionFiles: [], skillFiles: [] });
const tasks = [
  task("plan", "claude-code", "opus", "high", [], "running", "att-plan"),
  task("implement", "codex", "gpt-5.6-luna", "xhigh", ["plan"]),
  task("review", "claude-code", "opus", "high", ["implement"]),
  task("verify", "claude-code", "opus", "high", ["implement"], "running", "att-verify"), // concurrent with plan for demo purposes
  task("architect-review", "claude-code", "opus", "high", ["review", "verify"]),
  task("human-review", "", "", "", ["architect-review"]),
];
const run = { id: runId, projectId, launchKey: "guild-demo", sourceCommit: "4f1c2a9e8b7d6c5b4a39281706f5e4d3c2b1a090", bundleSha256: "", verificationSha256: "", state: "active", createdAt: now() };
const byKey = (key) => tasks.find((t) => t.key === key);
let reviewPackage = null;

// Durable event log + SSE subscribers (GET /api/runs/{id}/events?after=N).
const events = [];
const streams = new Set();
function emit(key, kind, payload) {
  const t = byKey(key);
  const event = { id: String(events.length + 1), runId, taskId: t?.id ?? "", attemptId: t?.activeAttemptId ?? "", kind, occurredAt: now(), ...(payload ? { payloadJson: JSON.stringify(payload) } : {}) };
  events.push(event);
  for (const res of streams) res.write(`id: ${event.id}\ndata: ${JSON.stringify(event)}\n\n`);
}
const progress = (key, payload) => emit(key, "attempt.progress", payload);
const setState = (key, state, attempt) => { const t = byKey(key); t.state = state; if (attempt !== undefined) { t.activeAttemptId = attempt; if (attempt) t.generation = String(Number(t.generation) + 1); } emit(key, state === "running" ? "attempt.started" : state === "succeeded" ? "attempt.result" : "task.created"); };

// Interactions.
const interactions = [];
const option = (id, label, description = "", recommended = false) => ({ id, label, description, recommended });
function open(stage, fields) {
  const item = { id: `ix-${interactions.length + 1}`, attemptId: byKey(stage).activeAttemptId, stage, options: [], multiSelect: false, allowFreeText: false, blocking: true, sources: [], bodyMd: "", state: "open", createdAt: now(), ...fields };
  interactions.push(item);
  emit(stage, "interaction.opened");
  return item;
}
const rounds = [
  { title: "Who is the primary user of the export feature?", options: [option("ops", "Operations admins", "Bulk exports for audits", true), option("customers", "End customers"), option("both", "Both")] },
  { title: "Which export formats must ship in v1?", multiSelect: true, bodyMd: "The transcript mentions CSV twice and JSON once.\nXLSX appears only in the stretch-goals list.", options: [option("csv", "CSV", "Spreadsheet-friendly", true), option("json", "JSON", "For API consumers"), option("xlsx", "XLSX")] },
  { title: "How long should generated exports be retained?", options: [option("1d", "24 hours"), option("7d", "7 days", "Matches the audit window in docs/spec.md", true), option("30d", "30 days")] },
  { title: "Anything else before I write the spec?", options: [] },
];
function interviewRound(n) {
  const r = rounds[n - 1];
  return open("plan", { kind: "interview_round", allowFreeText: true, interview: { round: n, finalizeOption: "done" }, ...r });
}

// Scenario.
progress("plan", { type: "phase", name: "R2 interview" });
const first = interviewRound(1);
Object.assign(first, { state: "answered", answer: { interactionId: first.id, optionIds: ["ops"], text: "Mostly the support ops team.", answeredBy: principalId, at: now() } });
emit("plan", "interaction.answered");
interviewRound(2);
open("plan", {
  kind: "approval", title: "Approve crew plan", allowFreeText: true,
  bodyMd: "## Crew plan\n1. implementer (codex): export handler + CSV/JSON writers\n2. reviewer: API + retention review\n3. verify loop: `go test ./...`, max 3 cycles\n\nRisk: retention job touches the shared scheduler.",
  sources: [{ path: "docs/spec.md", line: 12 }, { path: "internal/export/handler.go" }],
  options: [option("go", "Go", "Start implementation with this crew", true), option("revise", "Revise", "Ask the architect to rework the plan"), option("cancel", "Cancel", "Stop this run")],
});
let cap = 3;
let cycle = 0;
const findings = [["TestExportCSV: header row missing `created_at`", "retention job not registered with scheduler"], ["TestExportJSON: timestamps not RFC 3339"], ["TestRetention: flaky under -race (shared clock)"]];
function nextCycle() {
  cycle += 1;
  progress("verify", { type: "cycle", cycle, max_cycles: cap, status: "running" });
  void verifyWork(cycle);
  setTimeout(() => {
    for (const text of findings[(cycle - 1) % findings.length]) progress("verify", { type: "finding", cycle, text });
    const pass = cycle > 3;
    progress("verify", { type: "cycle", cycle, max_cycles: cap, status: pass ? "pass" : "fail" });
    if (pass) return finishVerify();
    if (cycle < cap) return setTimeout(nextCycle, 2_000);
    open("verify", {
      kind: "escalation", title: `Verify loop hit its cap (${cap} cycles)`, allowFreeText: true,
      bodyMd: `Cycle ${cycle} still fails:\n- ${findings[(cycle - 1) % findings.length].join("\n- ")}`,
      sources: [{ path: "internal/export/retention_test.go", line: 48 }],
      options: [option("raise", "Raise cap to 5", "Run two more cycles", true), option("accept", "Accept with exceptions"), option("halt", "Halt the loop")],
    });
  }, 3_000);
}
function finishVerify() {
  setState("verify", "succeeded", "");
  setState("architect-review", "succeeded");
  setState("human-review", "running");
  reviewPackage = { id: "pkg-1", runId, revision: "1", integratedCommit: "9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b", sourceCommit: run.sourceCommit, evidenceSha256: "e".repeat(64), verificationSha256: "f".repeat(64), bundleSha256: "b".repeat(64), presentedAt: now() };
  emit("human-review", "review.presented");
}
setTimeout(nextCycle, 1_500);

// Agent work log (docs/interactive-sessions.md, "Agent activity"): the normalized records the
// guest pane derives from harness JSON, replayed on a timer. Same id = same item, latest wins.
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
async function planWork() {
  const todo = (statuses) => progress("plan", { type: "plan.updated", id: "plan", items: [
    ["Read the export handler and spec", statuses[0]], ["Map CSV/JSON writer changes", statuses[1]], ["Draft crew plan", statuses[2]], ["Ask for plan approval", statuses[3]]].map(([text, status]) => ({ text, status })) });
  todo(["in_progress", "pending", "pending", "pending"]);
  progress("plan", { type: "assistant.message", id: "msg_1", text: "I'll read the export handler and the spec before drafting the crew plan." });
  for (const [id, tool, input, ms] of [["toolu_1", "Read", "internal/export/handler.go", 40], ["toolu_2", "Read", "docs/spec.md", 25], ["toolu_3", "Grep", "created_at", 12], ["toolu_4", "Glob", "internal/export/**/*_test.go", 8]]) {
    await sleep(700);
    progress("plan", { type: "tool.completed", id, tool, input, status: id === "toolu_3" ? "failed" : "completed", duration_ms: ms });
  }
  todo(["completed", "in_progress", "pending", "pending"]);
  await sleep(800);
  progress("plan", { type: "command", id: "toolu_5", cmd: "go test ./internal/export/... -run TestExport", status: "running" });
  await sleep(2_500);
  progress("plan", { type: "command", id: "toolu_5", cmd: "go test ./internal/export/... -run TestExport", status: "failed", exit: 1, duration_ms: 2_480 });
  await sleep(600);
  progress("plan", { type: "tool.started", id: "toolu_6", tool: "WebFetch", input: "https://www.rfc-editor.org/rfc/rfc3339", status: "running" });
  await sleep(1_800);
  progress("plan", { type: "tool.completed", id: "toolu_6", tool: "WebFetch", input: "https://www.rfc-editor.org/rfc/rfc3339", status: "completed", duration_ms: 1_790 });
  progress("plan", { type: "file.changed", id: "toolu_7", tool: "Write", path: "docs/plans/export-crew.md", status: "completed", added: 42, removed: 0 });
  todo(["completed", "completed", "completed", "in_progress"]);
  progress("plan", { type: "assistant.message", id: "msg_2", text: "Crew plan drafted in docs/plans/export-crew.md. Waiting for approval before implementation starts.\n\nRisk: the retention job touches the shared scheduler.", truncated: false });
}
async function verifyWork(n) {
  progress("verify", { type: "command", id: `cmd-${n}`, cmd: "go test ./...", status: "running" });
  await sleep(1_500);
  progress("verify", { type: "command", id: `cmd-${n}`, cmd: "go test ./...", status: n > 3 ? "completed" : "failed", exit: n > 3 ? 0 : 1, duration_ms: 1_480 });
  progress("verify", { type: "file.changed", id: `edit-${n}`, tool: "Edit", path: "internal/export/csv.go", status: "completed", added: 3, removed: 1 });
  progress("verify", { type: "file.changed", id: `edit-${n}b`, tool: "Edit", path: "internal/export/retention_test.go", status: "completed", added: 12, removed: 4 });
}
void planWork();

function answered(item, answer) {
  if (item.kind === "interview_round") {
    if (answer.optionIds.includes("done") || item.interview.round >= rounds.length) progress("plan", { type: "phase", name: "Interview finalized; writing spec" });
    else setTimeout(() => interviewRound(item.interview.round + 1), 1_200);
  }
  if (item.kind === "approval") progress("plan", { type: "progress", message: `Crew plan: ${answer.optionIds.join(", ") || answer.text}` });
  if (item.kind === "escalation") {
    if (answer.optionIds.includes("raise")) { cap = 5; setTimeout(nextCycle, 1_000); }
    else if (answer.optionIds.includes("takeover")) progress("verify", { type: "progress", message: "Waiting for a human in the terminal" });
    else { progress("verify", { type: "progress", message: `Loop closed: ${answer.optionIds.join(", ")}` }); finishVerify(); }
  }
}

// Terminal control per attempt + WebSocket fan-out.
const control = new Map(); // attemptId -> holder principal or null
const sockets = new Set();
const stateFrame = (attemptId) => JSON.stringify({ type: "state", control: control.get(attemptId) ? "human" : "agent", holder: control.get(attemptId) ?? null, stage: tasks.find((t) => t.activeAttemptId === attemptId)?.key ?? "", attemptStatus: "running" });
const broadcast = (attemptId) => { for (const s of sockets) if (s.attemptId === attemptId) s.send(stateFrame(attemptId)); };
const agentLines = ["\x1b[36m⏺\x1b[0m Read(internal/export/handler.go)", "  ⎿  Read 142 lines", "\x1b[36m⏺\x1b[0m Bash(go test ./internal/export/...)", "  ⎿  \x1b[31mFAIL\x1b[0m TestExportCSV (0.02s)", "\x1b[36m⏺\x1b[0m Edit(internal/export/csv.go)", "  ⎿  Updated with 3 additions", "\x1b[2m✻ Thinking…\x1b[0m"];

// Connect unary JSON handlers.
const connectError = (status, code, message) => ({ status, body: { code, message } });
const hub = [];
let deviceStarted = 0;
let gitHubApp = { clientId: "", configured: false };
function hubAdd(fields) {
  const connection = { id: `conn-${hub.length + 1}`, ownerName: "", label: "", state: "active", grants: [], uses: [], lastUsedAt: "", createdAt: now(), modelCount: 0, modelsCheckedAt: "", modelsError: "", canManage: true, ...fields };
  connection.health = { state: "ready", auth: fields.kind === "git" ? "unknown" : "authenticated", identity: connection.kind === "api_key" ? connection.label : connection.account,
    checkedAt: connection.kind === "git" ? "" : now(), reason: "", message: "", harness: "", pinnedVersion: "", latestVersion: "", ...fields.health };
  hub.push(connection);
  return { connection };
}
// Seeded hub: one healthy granted key, one rejected key, a stale-runtime key, Git, and a personal login that needs sign-in.
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "demo", kind: "api_key", provider: "anthropic", label: "Anthropic prod", modelCount: 7, modelsCheckedAt: minutesAgo(42),
  grants: [{ id: "grant-seed-project", projectId, projectName: "Demo project", granteeKind: "project", granteeId: "", granteeName: "", createdAt: minutesAgo(600) }],
  uses: [{ id: "use-seed", projectId, projectName: "Demo project", model: "claude-opus-5", granteeKind: "workload", createdAt: minutesAgo(500) }],
  health: { checkedAt: minutesAgo(42), harness: "claude-code", pinnedVersion: "2.1.10", latestVersion: "2.1.10" } });
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "demo", kind: "api_key", provider: "openai", label: "OpenAI sandbox", modelsCheckedAt: minutesAgo(5), modelsError: "the provider rejected this API key",
  grants: [{ id: "grant-seed-role", projectId: "", projectName: "", granteeKind: "role", granteeId: "member", granteeName: "", createdAt: minutesAgo(900) }],
  health: { state: "error", auth: "unauthenticated", reason: "key_rejected", message: "The provider rejected this API key; replace it.", checkedAt: minutesAgo(5), harness: "codex" } });
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "demo", kind: "api_key", provider: "opencode", label: "Zen team", modelCount: 12, modelsCheckedAt: minutesAgo(180),
  health: { state: "warning", reason: "harness_behind", message: "Pinned opencode 0.9.2 is behind the catalog's latest 1.0.0.", checkedAt: minutesAgo(180), harness: "opencode", pinnedVersion: "0.9.2", latestVersion: "1.0.0" } });
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "demo", kind: "git", provider: "github", account: "blaxsmith-bot" });
hubAdd({ scope: "personal", ownerId: principalId, ownerName: "you", kind: "subscription", provider: "codex", account: "acct-7f3c2e", state: "reconnect_required", modelCount: 3,
  health: { state: "error", auth: "unauthenticated", reason: "needs_sign_in", message: "The saved sign-in stopped working; sign in again.", checkedAt: minutesAgo(60 * 26), harness: "codex" } });
const rpc = {
  GetCsrf: () => ({ token: "A".repeat(43) }),
  CurrentSession: () => ({ session: { organizationId: "org-demo", principalId, role: "owner", accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() } }),
  RefreshSession: () => rpc.CurrentSession(),
  GetProject: () => ({ project: { id: projectId, slug: "demo", name: "Demo project", createdAt: now() } }),
  GetProjectSource: () => ({ source: { projectId, repositoryUrl: "https://github.com/example/blaxsmith-demo.git", ref: "main" } }),
  GetProjectVerification: () => ({ verification: { projectId, version: "1", checks: [{ id: "go-test", command: ["go", "test", "./..."] }] } }),
  GetLaunchAvailability: () => ({ enabled: true, reason: "" }),
  LaunchRun: () => ({ run }),
  ListRuns: () => ({ runs: [run], nextPageToken: "" }),
  ListProjects: () => ({ projects: [rpc.GetProject().project], nextPageToken: "" }),
  GetRun: () => ({ run }),
  ListRunTasks: () => ({ tasks }),
  EventsAfter: ({ afterId = "0", limit = 100 }) => { const page = events.filter((e) => Number(e.id) > Number(afterId)).slice(0, limit); return { events: page, nextAfterId: page.at(-1)?.id ?? String(afterId) }; },
  ListCommandExits: () => ({ observations: [], nextAfterEventId: "0" }),
  GetCurrentReview: () => reviewPackage ? { package: reviewPackage } : connectError(404, "not_found", "no review package"),
  DecideReview: ({ action, feedback }) => {
    if (!reviewPackage || reviewPackage.decision) return connectError(400, "failed_precondition", "package already decided");
    reviewPackage.decision = { id: "dec-1", packageId: reviewPackage.id, principalId, action, feedback: feedback ?? "", decidedAt: now() };
    emit("human-review", action === "approve" ? "review.approved" : "review.changes_requested");
    if (action === "approve") { setState("human-review", "succeeded"); run.state = "succeeded"; emit("", "run.succeeded"); }
    else { setState("human-review", "pending"); setState("implement", "running", "att-implement-2"); }
    return { decision: reviewPackage.decision };
  },
  ListInteractions: () => ({ interactions }),
  AnswerInteraction: ({ interactionId, optionIds = [], text = "" }) => {
    const item = interactions.find((i) => i.id === interactionId);
    if (!item) return connectError(404, "not_found", "no such interaction");
    if (item.state !== "open") return connectError(400, "failed_precondition", "already answered");
    item.state = "answered";
    item.answer = { interactionId, optionIds, text, answeredBy: principalId, at: now() };
    emit(item.stage, "interaction.answered");
    answered(item, item.answer);
    return { interaction: item };
  },
  SteerAttempt: ({ attemptId, kind, text, reason, n }) => {
    const stage = tasks.find((t) => t.activeAttemptId === attemptId)?.key;
    if (!stage) return connectError(400, "failed_precondition", "attempt is not running");
    if (kind === "set_max_cycles") cap = Number(n);
    progress(stage, { type: "progress", message: `Steering queued: ${kind}${text ? ` "${text}"` : ""}${reason ? ` (${reason})` : ""}${n ? ` → ${n}` : ""}` });
    return {};
  },
  GetAttemptControl: () => ({ canTakeOver: true }),
  TakeOverAttempt: ({ attemptId }) => {
    if (control.get(attemptId)) return connectError(400, "failed_precondition", "another principal holds control");
    control.set(attemptId, principalId);
    broadcast(attemptId);
    emit(tasks.find((t) => t.activeAttemptId === attemptId)?.key, "attempt.control");
    return {};
  },
  HandBackAttempt: ({ attemptId }) => {
    control.set(attemptId, null);
    broadcast(attemptId);
    emit(tasks.find((t) => t.activeAttemptId === attemptId)?.key, "attempt.control");
    return {};
  },
  // RecipeService: an in-memory library seeded with the Guild recipe. The
  // mock only checks JSON syntax; the real server runs internal/recipe validation.
  ListRecipes: ({ projectId: project = "" }) => ({ recipes: recipes.filter((r) => !r.projectId || r.projectId === project).map(recipeSummary) }),
  GetRecipe: ({ recipeId }) => { const r = recipes.find((x) => x.id === recipeId); return r ? { recipe: recipeSummary(r), versions: [...r.versions].reverse().map(({ recipeJson, ...v }) => v), grants: r.projectId ? [] : r.grants || [] } : connectError(404, "not_found", "workflow resource not found"); },
  GetRecipeVersion: ({ versionId }) => { const v = recipes.flatMap((r) => r.versions).find((x) => x.id === versionId); return v ? { version: v } : connectError(404, "not_found", "workflow resource not found"); },
  ValidateRecipe: ({ recipeJson = "" }) => { try { const doc = JSON.parse(recipeJson); return { errors: [], stageOrder: (doc.stages || []).map((x) => x.id) }; } catch (e) { return { errors: [{ path: "$", message: String(e.message) }], stageOrder: [] }; } },
  CreateRecipe: ({ projectId: project = "", name, description = "", recipeJson, frozenPath = "" }) => { const r = { id: `recipe-${recipes.length + 1}`, projectId: project, name, description, createdAt: now(), versions: [] }; recipes.push(r); addVersion(r, recipeJson, frozenPath, true); return { recipe: recipeSummary(r), version: r.versions[0] }; },
  CreateRecipeVersion: ({ recipeId, recipeJson, frozenPath = "", makeCurrent = false }) => { const r = recipes.find((x) => x.id === recipeId); return { version: addVersion(r, recipeJson, frozenPath, makeCurrent) }; },
  SetCurrentRecipeVersion: ({ recipeId, versionId }) => { const r = recipes.find((x) => x.id === recipeId); r.current = versionId; return { recipe: recipeSummary(r) }; },
  CloneRecipe: ({ sourceVersionId, projectId: project = "", name, description = "" }) => { const v = recipes.flatMap((r) => r.versions).find((x) => x.id === sourceVersionId); return rpc.CreateRecipe({ projectId: project, name, description, recipeJson: v.recipeJson, frozenPath: v.frozenPath }); },
  GrantRecipe: ({ recipeId, projectId: pid = "", granteeKind, granteeId = "" }) => { const r = recipes.find((x) => x.id === recipeId); if (!r || r.projectId) return connectError(400, "invalid_argument", "invalid workflow request"); const grant = { id: `rgrant-${Date.now()}`, projectId: pid, projectName: pid ? "Demo project" : "", granteeKind, granteeId, granteeName: granteeKind === "user" ? "teammate" : "", createdAt: now() }; (r.grants ||= []).push(grant); return { grant }; },
  RevokeRecipeGrant: ({ grantId }) => { for (const r of recipes) r.grants = (r.grants || []).filter((g) => g.id !== grantId); return {}; },
  GetRecipeEditorOptions: () => ({
    harnesses: [{ harness: "claude-code", provider: "anthropic", efforts: ["low", "medium", "high", "xhigh", "max"] }, { harness: "codex", provider: "openai", efforts: ["minimal", "low", "medium", "high", "xhigh"] }, { harness: "opencode", provider: "", efforts: ["provider-default", "low", "medium", "high"] }],
    stageKinds: ["plan", "interview", "research", "implement", "review", "verify", "integrate", "ui_review", "documentation", "architect_review", "human_review"],
    connections: [{ id: "conn-openai", provider: "openai", account: "platform-team", models: ["gpt-5.6-luna", "gpt-5.6"] }, { id: "conn-anthropic", provider: "anthropic", account: "eng", models: ["opus", "sonnet"] }],
  }),
  ListProjectRecipeFiles: () => ({ commit: run.sourceCommit, skillPaths: ["examples/guild/skills/evidence/SKILL.md", "skills/security/SKILL.md"],
    promptPaths: ["examples/guild/prompts/plan.md", "examples/guild/prompts/implement.md", "examples/guild/prompts/review.md", "examples/guild/prompts/verify.md", "examples/guild/prompts/architect-review.md"] }),
  ListTools: () => ({ tools: ["codex", "claude-code", "opencode"].map((tool) => ({ tool, package: tool, source: "mock", fetchedAt: now(), latestStable: "1.0.0", releases: [] })), stale: false }),
  // AdminService: derived from the scenario above plus a second, stuck run.
  GetAdminOverview: () => {
    const project = { projectId, projectName: "Demo project", runId, runLaunchKey: run.launchKey };
    const liveAttempts = tasks.filter((t) => t.activeAttemptId && run.state === "active").map((t) => ({ ...project, attemptId: t.activeAttemptId, stage: t.key,
      kind: t.key === "plan" ? "plan" : t.key === "verify" ? "verify" : "implement", harness: t.harness, model: t.model, state: "running",
      controllerPrincipalId: control.get(t.activeAttemptId) ?? "", controllerUsername: control.get(t.activeAttemptId) ? "you" : "",
      startedAt: minutesAgo(t.key === "plan" ? 14 : 6), lastActivityAt: events.filter((e) => e.attemptId === t.activeAttemptId).at(-1)?.occurredAt ?? "" }));
    liveAttempts.push({ attemptId: "att-stuck", runId: "run-stuck", projectId: "proj-billing", projectName: "Billing service", runLaunchKey: "invoice-retry-fix",
      stage: "implement", kind: "implement", harness: "codex", model: "gpt-5.6-luna", state: "reconciling", controllerPrincipalId: "", controllerUsername: "",
      startedAt: minutesAgo(190), lastActivityAt: minutesAgo(47) });
    const openInteractions = interactions.filter((i) => i.state === "open").map((i) => ({ ...project, id: i.id, stage: i.stage, kind: i.kind, title: i.title, blocking: i.blocking, createdAt: i.createdAt }));
    return {
      liveAttempts, openInteractions, generatedAt: now(),
      runStates: [["running", run.state === "active" ? 2 : 1], ["waiting_on_human", openInteractions.length ? 1 : 0], ["escalated", interactions.some((i) => i.state === "open" && i.kind === "escalation") ? 1 : 0], ["failed", 1], ["succeeded", 4], ["halted", adminHalted.size]].map(([state, count]) => ({ state, count })),
      capacity: { inFlight: liveAttempts.length, running: liveAttempts.filter((a) => a.state === "running").length, takenOver: liveAttempts.filter((a) => a.controllerPrincipalId).length, configuredMax: 8, workerPool: "dev-pool" },
      connections: [
        { id: "conn-openai", providerKind: "openai", host: "api.openai.com", account: "unverified-openai-api-key", ownerKind: "organization", state: "active", activeGrants: adminGrants.filter((g) => g.connectionId === "conn-openai").length, activeLeases: 2, expiringLeases: 1, lastUsedAt: minutesAgo(1), createdAt: minutesAgo(60 * 24 * 9) },
        { id: "conn-git", providerKind: "git", host: "github.com", account: "blaxsmith-bot", ownerKind: "organization", state: "active", activeGrants: adminGrants.filter((g) => g.connectionId === "conn-git").length, activeLeases: 1, expiringLeases: 0, lastUsedAt: minutesAgo(6), createdAt: minutesAgo(60 * 24 * 12) },
        { id: "conn-old", providerKind: "anthropic", host: "api.anthropic.com", account: "unverified-anthropic-api-key", ownerKind: "organization", state: "revoked", activeGrants: 0, activeLeases: 0, expiringLeases: 0, lastUsedAt: "", createdAt: minutesAgo(60 * 24 * 30) },
      ],
      grants: adminGrants,
    };
  },
  ListAuditEvents: ({ pageToken = "", action = "", actor = "", projectId: project = "", pageSize = 50 }) => {
    const before = pageToken ? Number(atob(pageToken)) : Infinity;
    const rows = audit.filter((e) => Number(e.id) < before && (!action || e.action === action) && (!actor || e.actorUsername === actor || e.actorId === actor) && (!project || e.projectId === project));
    const page = rows.slice(0, pageSize);
    return { events: page, nextPageToken: rows.length > pageSize ? btoa(page.at(-1).id) : "" };
  },
  HaltRun: ({ runId: id }) => {
    adminHalted.add(id);
    if (id === runId) { run.state = "cancel_requested"; emit("", "run.cancel_requested"); }
    auditEvent("workflow.run.halted", id);
    return { runId: id, state: "cancel_requested" };
  },
  RevokeGrant: ({ grantId }) => {
    const index = adminGrants.findIndex((g) => g.id === grantId);
    if (index < 0) return connectError(404, "not_found", "workflow resource not found");
    adminGrants.splice(index, 1);
    auditEvent("access.grant.revoked", grantId);
    return { grantId };
  },
  // ConnectionService: an in-memory hub. No handler ever echoes a secret.
  ListConnections: ({ scope = "", projectId: pid = "" }) => ({ connections: hub.filter((c) => scope === "project_available" ? c.scope === "organization" && c.grants.some((g) => g.projectId === pid) : c.scope === scope && (scope !== "project" || c.ownerId === pid)) }),
  CreateApiKeyConnection: ({ scope, projectId: pid = "", provider, label = "" }) => hubAdd({ scope, ownerId: scope === "project" ? pid : scope === "personal" ? principalId : "org-demo", kind: "api_key", provider, label, account: `${provider} key`, modelCount: 3, modelsCheckedAt: now() }),
  CreateGitTokenConnection: ({ scope, projectId: pid = "", host, username }) => hubAdd({ scope, ownerId: scope === "project" ? pid : "org-demo", kind: "git", provider: host === "gitlab.com" ? "gitlab" : "github", account: username }),
  CreateCodexSubscription: () => hubAdd({ scope: "personal", ownerId: principalId, kind: "subscription", provider: "codex", account: "acct-demo", modelCount: 2, modelsCheckedAt: now() }),
  StartCodexDeviceLogin: () => { deviceStarted = Date.now(); return { loginId: "login-1", verificationUrl: "https://auth.openai.com/codex/device", userCode: "ABCD-1234", intervalSeconds: 2, expiresAt: new Date(Date.now() + 900_000).toISOString() }; },
  PollCodexDeviceLogin: () => Date.now() - deviceStarted < 6_000 ? { state: "pending" } : { state: "connected", connection: rpc.CreateCodexSubscription().connection },
  ListConnectionModels: ({ connectionId, harness = "" }) => ({ checkedAt: now(), models: mockModels.filter((m) => !harness || m.harnesses.includes(harness))
    .map((m) => ({ ...m, recommended: (recommended.get(connectionId) ?? ["claude-opus-5"]).includes(m.id), efforts: harness === "codex" ? m.efforts.filter((e) => e !== "max") : m.efforts })) }),
  SetRecommendedModels: ({ connectionId, models = [] }) => { recommended.set(connectionId, models); return { models }; },
  RefreshConnectionModels: () => ({ valid: true, modelCount: 2, checkedAt: now() }),
  GrantConnection: ({ connectionId, projectId: pid = "", granteeKind, granteeId = "" }) => { const grant = { id: `grant-${hub.length}-${Date.now()}`, projectId: pid, projectName: pid ? "Demo project" : "", granteeKind, granteeId, createdAt: now() }; hub.find((c) => c.id === connectionId)?.grants.push(grant); return { grant }; },
  RevokeConnectionGrant: ({ grantId }) => { for (const c of hub) c.grants = c.grants.filter((g) => g.id !== grantId); return {}; },
  AddConnectionUse: ({ connectionId, projectId: pid, model }) => { const c = hub.find((x) => x.id === connectionId); const use = { id: `use-${Date.now()}`, projectId: pid, projectName: "Demo project", model, granteeKind: c?.scope === "personal" ? "user" : "workload", createdAt: now() }; c?.uses.push(use); return { use }; },
  RemoveConnectionUse: ({ useId }) => { for (const c of hub) c.uses = c.uses.filter((u) => u.id !== useId); return {}; },
  RevokeConnection: ({ connectionId }) => { const c = hub.find((x) => x.id === connectionId); if (c) c.state = "revoked"; return {}; },
  ListGitRepositories: ({ query = "" }) => ({ repositories: [{ fullName: "example/blaxsmith-demo", cloneUrl: "https://github.com/example/blaxsmith-demo.git", defaultBranch: "main", private: true }, { fullName: "example/billing", cloneUrl: "https://github.com/example/billing.git", defaultBranch: "trunk" }].filter((r) => r.fullName.includes(query)) }),
  ListGitBranches: () => ({ branches: ["main", "develop", "release/1.0"] }),
  GetGitHubApp: () => ({ clientId: gitHubApp.clientId, configured: gitHubApp.configured, callbackUrl: "http://localhost:5173/oauth/github/callback" }),
  SetGitHubApp: ({ clientId, clientSecret = "" }) => { gitHubApp = { clientId, configured: gitHubApp.configured || Boolean(clientSecret) }; return gitHubApp; },
  ListOrgMembers: () => ({ members: [{ principalId, username: "you", displayName: "You", role: "owner", state: "active", createdAt: minutesAgo(60 * 24 * 30) },
    { principalId: "p-mara", username: "mara", displayName: "Mara Lin", role: "member", state: "active", createdAt: minutesAgo(60 * 24 * 3) }] }),
  ListProjectModelAccess: () => ({ access: [] }),
  InspectRepository: () => ({ commit: run.sourceCommit, source: "file", fileError: "", recipe: "Guild engineering", evidence: [".blaxsmith.json"],
    verification: [{ id: "go-test", command: ["go", "test", "./..."] }, { id: "go-vet", command: ["go", "vet", "./..."] }, { id: "lint", command: ["make", "lint"] }],
    setup: [{ id: "go-mod-download", command: ["go", "mod", "download"] }] }),
  // SetupService: a simplified grant walk; the server runs access.MatchingGrant.
  ExplainAccess: ({ projectId: pid, resourceKind, resourceId, principalId: who = "" }) => {
    const reach = { member: "members and above", admin: "admins and owners", owner: "owners only" };
    const chain = (grants, resource) => {
      const g = grants.find((x) => x.projectId === pid) || grants.find((x) => x.granteeKind === "user" && (!who || x.granteeId === who)) || grants.find((x) => x.granteeKind === "role");
      if (!g) return { usable: false, steps: [], reason: "No grant for this project, user, or role; ask an organization admin.", principalLabel: who ? "teammate" : "you" };
      const kind = `${g.granteeKind}_grant`;
      return { usable: true, grantId: g.id, principalLabel: who ? "teammate" : "you", steps: [{ kind, label: g.granteeKind === "project" ? g.projectName : g.granteeKind === "role" ? reach[g.granteeId] : "teammate", id: g.id }, resource] };
    };
    if (resourceKind === "recipe") {
      const r = recipes.find((x) => x.id === resourceId);
      if (!r) return connectError(404, "not_found", "workflow resource not found");
      const version = { kind: "recipe_version", label: `v${recipeSummary(r).currentVersion} (current)`, id: r.current };
      if (r.projectId) return { usable: r.projectId === pid, steps: [{ kind: "project_recipe", label: r.name, id: r.id }, version], reason: "It belongs to another project." };
      const out = chain(r.grants || [], { kind: "organization_recipe", label: r.name, id: r.id });
      if (out.usable) out.steps.push(version);
      return out;
    }
    const c = hub.find((x) => x.id === resourceId);
    if (!c) return connectError(404, "not_found", "workflow resource not found");
    if (c.scope === "personal") return { usable: !who, steps: who ? [] : [{ kind: "personal_connection", label: c.label || c.provider, id: c.id }], reason: "Personal connections serve only runs their owner launches." };
    if (c.scope === "project") return { usable: c.ownerId === pid, steps: [{ kind: "project_connection", label: c.label || c.provider, id: c.id }], reason: "It belongs to another project." };
    return chain(c.grants, { kind: "organization_connection", label: c.label || c.provider, id: c.id });
  },
  StartGitHubConnect: ({ returnTo = "/admin/connections" }) => { hubAdd({ scope: "organization", ownerId: "org-demo", kind: "git", provider: "github", account: "octocat" }); return { authorizeUrl: `${returnTo}?github=connected` }; },
};

const mockModel = (id, displayName, harnesses, efforts, defaultEffort, extra = {}) => ({ id, displayName, contextTokens: 400000, harnesses, efforts, defaultEffort, isDefault: false, legacy: false, badge: "", ...extra });
const mockModels = [
  mockModel("gpt-6-sol", "GPT-6 Sol", ["codex", "opencode"], ["low", "medium", "high", "xhigh"], "medium", { badge: "new" }),
  mockModel("gpt-6-luna", "GPT-6 Luna", ["codex", "opencode"], ["low", "medium", "high", "xhigh"], "medium", { isDefault: true }),
  mockModel("gpt-5", "GPT-5", ["codex", "opencode"], ["minimal", "low", "medium", "high"], "medium", { legacy: true }),
  mockModel("claude-opus-5-5", "Claude Opus 5.5", ["claude-code", "opencode"], ["low", "medium", "high", "xhigh", "max"], "medium", { badge: "new" }),
  mockModel("claude-opus-5", "Claude Opus 5", ["claude-code", "opencode"], ["low", "medium", "high", "xhigh", "max"], "high", { isDefault: true }),
  mockModel("claude-opus-4-6", "Claude Opus 4.6", ["claude-code", "opencode"], ["low", "medium", "high", "max"], "high", { legacy: true }),
  mockModel("claude-haiku-4-5", "Claude Haiku 4.5", ["claude-code", "opencode"], [], "", { legacy: true, contextTokens: 200000 }),
];
const recommended = new Map();
const recipes = [];
function addVersion(r, recipeJson, frozenPath, makeCurrent) {
  const doc = (() => { try { return JSON.parse(recipeJson); } catch { return {}; } })();
  const v = { id: `${r.id}-v${r.versions.length + 1}`, recipeId: r.id, version: r.versions.length + 1, recipeJson, sha256: [...recipeJson].reduce((h, c) => (h * 31 + c.charCodeAt(0)) >>> 0, 7).toString(16).padStart(64, "0"),
    frozenPath: frozenPath || `.blaxsmith/recipes/${doc.name || "recipe"}.json`, authorPrincipalId: principalId, authorUsername: "you", createdAt: now() };
  r.versions.push(v);
  if (makeCurrent) r.current = v.id;
  r.updatedAt = now();
  return v;
}
const recipeSummary = (r) => ({ id: r.id, projectId: r.projectId, name: r.name, description: r.description, currentVersionId: r.current || "",
  currentVersion: r.versions.find((v) => v.id === r.current)?.version || 0, versionCount: r.versions.length, createdAt: r.createdAt, updatedAt: r.updatedAt || r.createdAt });
{
  const seed = { id: "recipe-guild", projectId: "", name: "Guild engineering", description: "Plan, implement, review, verify with a bounded correction loop, architect review, then human review.", createdAt: minutesAgo(60 * 24 * 20), versions: [] };
  recipes.push(seed);
  addVersion(seed, readFileSync(new URL("../../examples/guild/recipe.json", import.meta.url), "utf8"), "examples/guild/recipe.json", true);
  seed.versions[0].authorUsername = ""; seed.versions[0].authorPrincipalId = "";
  seed.grants = [{ id: "rgrant-seed", projectId: "", projectName: "", granteeKind: "role", granteeId: "member", granteeName: "", createdAt: seed.createdAt }];
}

function minutesAgo(n) { return new Date(Date.now() - n * 60_000).toISOString(); }
const adminHalted = new Set();
const adminGrants = [
  { id: "grant-model", connectionId: "conn-openai", projectId, projectName: "Demo project", capability: "model.invoke", resource: "openai/gpt-5.6-luna", expiresAt: "", createdAt: minutesAgo(60 * 24 * 9) },
  { id: "grant-git-read", connectionId: "conn-git", projectId, projectName: "Demo project", capability: "git.read", resource: "https://github.com/example/blaxsmith-demo.git", expiresAt: "", createdAt: minutesAgo(60 * 24 * 12) },
  { id: "grant-git-write", connectionId: "conn-git", projectId, projectName: "Demo project", capability: "git.write", resource: "https://github.com/example/blaxsmith-demo.git", expiresAt: "", createdAt: minutesAgo(60 * 24 * 12) },
];
const audit = [];
function auditEvent(action, subjectId, actorUsername = "you", minutes = 0) {
  audit.unshift({ id: String(audit.length + 1), action, actorKind: "principal", actorId: actorUsername === "you" ? principalId : `p-${actorUsername}`, actorUsername, subjectId, projectId: action.startsWith("identity.") || action === "access.git_connection.created" ? "" : projectId, projectName: action.startsWith("identity.") || action === "access.git_connection.created" ? "" : "Demo project", occurredAt: minutesAgo(minutes) });
}
for (const [i, action] of ["installation.bootstrap_owner", "identity.login", "workflow.project.created", "access.git_connection.created", "workflow.project_source.set", "access.project_model.created", "workflow.project_verification.set", "identity.login", "workflow.run.launched", "workflow.interaction.answered", "workflow.attempt.steered", "identity.refresh"].entries()) {
  auditEvent(action, `00000000-0000-4000-8000-${String(i).padStart(12, "0")}`, i % 3 === 1 ? "mara" : "you", 600 - i * 45);
}

const server = createHttp(async (req, res) => {
  const url = new URL(req.url, "http://localhost");
  if (req.method === "GET" && url.pathname === `/api/runs/${runId}/events`) {
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    const after = Number(req.headers["last-event-id"] ?? url.searchParams.get("after") ?? 0);
    for (const e of events.filter((e) => Number(e.id) > after)) res.write(`id: ${e.id}\ndata: ${JSON.stringify(e)}\n\n`);
    streams.add(res);
    const ping = setInterval(() => res.write(": ping\n\n"), 15_000);
    req.on("close", () => { streams.delete(res); clearInterval(ping); });
    return;
  }
  const method = url.pathname.match(/^\/api\/blaxsmith\.api\.v1\.\w+\/(\w+)$/)?.[1];
  let body = "";
  for await (const chunk of req) body += chunk;
  const handler = method && rpc[method];
  const result = handler ? handler(body ? JSON.parse(body) : {}) : connectError(404, "unimplemented", `mock has no ${method ?? url.pathname}`);
  const [status, payload] = result?.status ? [result.status, result.body] : [200, result];
  res.writeHead(status, { "content-type": "application/json" });
  res.end(JSON.stringify(payload));
});

const wss = new WebSocketServer({ noServer: true });
server.on("upgrade", (req, socket, head) => {
  const attemptId = decodeURIComponent(new URL(req.url, "http://localhost").pathname.match(/^\/api\/terminal\/attempts\/([^/]+)$/)?.[1] ?? "");
  if (!attemptId) return socket.destroy();
  wss.handleUpgrade(req, socket, head, (ws) => {
    ws.attemptId = attemptId;
    if (!tasks.some((t) => t.activeAttemptId === attemptId)) { ws.send(JSON.stringify({ type: "error", message: "This attempt has no live session." })); return ws.close(); }
    sockets.add(ws);
    const out = (text) => ws.send(Buffer.from(text));
    ws.send(stateFrame(attemptId));
    const human = control.get(attemptId) === principalId;
    out(`\x1b[2J\x1b[H\x1b[1mblaxsmith\x1b[0m attempt ${attemptId} · ${human ? "\x1b[32minteractive (claude --resume)\x1b[0m" : "\x1b[2mread-only view\x1b[0m"}\r\n\r\n`);
    if (human) out("\x1b[35m>\x1b[0m ");
    let line = 0;
    const tick = setInterval(() => { if (!control.get(attemptId)) out(`${agentLines[line++ % agentLines.length]}\r\n`); }, 1_200);
    ws.on("message", (data, isBinary) => {
      if (!isBinary) { const frame = JSON.parse(String(data)); if (frame.type === "resize") console.log(`[mock] resize ${attemptId} ${frame.cols}x${frame.rows}`); return; }
      if (control.get(attemptId) !== principalId) return; // server drops bytes from non-holders
      const text = String(data);
      out(text === "\r" ? "\r\n\x1b[2m(sent to agent)\x1b[0m\r\n\x1b[35m>\x1b[0m " : text === "\x7f" ? "\b \b" : text);
    });
    ws.on("close", () => { clearInterval(tick); sockets.delete(ws); });
  });
});

// MOCK_API_PORT / MOCK_WEB_PORT let parallel checkouts run their own mock.
const apiPort = Number(process.env.MOCK_API_PORT || 8001);
const webPort = Number(process.env.MOCK_WEB_PORT || 3000);
server.listen(apiPort, "127.0.0.1", async () => {
  const vite = await createVite({ root: new URL("..", import.meta.url).pathname,
    server: { port: webPort, strictPort: true, proxy: { "/api": { target: `http://127.0.0.1:${apiPort}`, ws: true } } } });
  await vite.listen();
  console.log(`[mock] API on http://127.0.0.1:${apiPort} · open http://127.0.0.1:${webPort}/projects/${projectId}/runs/${runId} or /admin`);
});
