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
const claudePolicy = { allowed: process.env.MOCK_CLAUDE_SUB === "1" };
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
const guildManifest = readFileSync(new URL("../../examples/extensions/guild/blaxsmith-extension.json", import.meta.url), "utf8");
const extensions = [];
const extensionSummary = (e) => { const v = e.versions.find((x) => x.id === e.current); return { id: e.id, key: e.key, repositoryUrl: e.repositoryUrl, gitRef: e.gitRef,
  currentVersionId: v?.id || "", currentVersion: v?.version || "", currentCommit: v?.commit || "", latestRefCommit: e.latestRefCommit || "", updateAvailable: Boolean(v && e.latestRefCommit && e.latestRefCommit !== v.commit),
  versionCount: e.versions.length, latestCheckedAt: e.checkedAt || "", createdAt: e.createdAt, updatedAt: now() }; };
function extensionPreview(source) {
  let m;
  try { m = JSON.parse(source.overlayManifestJson || guildManifest); } catch (e) { return { errors: [{ path: "$", message: String(e.message) }], permissions: [], templates: [] }; }
  const permissions = [
    ...(m.native_subagents ? [{ id: "subagents", kind: "subagents", description: "Embedded templates may use the harness's own subagents", optional: false }] : []),
    ...(m.mcp_servers || []).map((x) => ({ id: `mcp:${x.id}`, kind: "mcp", description: `MCP server ${x.id}: ${x.purpose || ""}`, optional: Boolean(x.optional) })),
    ...(m.hooks || []).map((x) => ({ id: `hook:${x.id}`, kind: "hook", description: `${x.event} hook: ${x.purpose || ""}`, optional: Boolean(x.optional) })),
    ...(m.egress || []).map((x) => ({ id: `egress:${x.host}`, kind: "egress", description: `Sandbox egress to ${x.host}:${x.port}: ${x.purpose || ""}`, optional: false })),
    ...(m.runtimes || []).map((x) => ({ id: `runtime:${x.id}`, kind: "runtime", description: `Runner runtime layer ${x.id} ${x.version}`, optional: false })),
  ];
  const templates = (m.stage_templates || []).map((t) => ({ id: t.id, title: t.title, mode: t.mode, harness: t.harness, kinds: t.kinds, reference: `${m.id}@${m.version}/${t.id}` }));
  return { commit: "5f14799".padEnd(40, "0"), extensionKey: m.id, version: m.version, manifestJson: JSON.stringify(m, null, 2), manifestSha256: "d".repeat(64), permissions, templates, errors: [],
    existingExtensionId: extensions.find((x) => x.key === m.id)?.id || "" };
}

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
  // ExtensionService: previews derive permissions from the example Guild
  // manifest (or an overlay); the real server validates against the Git tree.
  // With a project, only extensions a grant lets this session use there (CanUse).
  ListExtensions: ({ projectId: pid = "" }) => ({ extensions: extensions.filter((e) => !pid || mayUseExtension(e, pid)).map(extensionSummary) }),
  GetExtension: ({ extensionId }) => { const e = extensions.find((x) => x.id === extensionId); return e ? { extension: extensionSummary(e), versions: [...e.versions].reverse(), grants: mayDecide() ? e.grants : [] } : connectError(404, "not_found", "workflow resource not found"); },
  PreviewExtensionInstall: ({ source = {} }) => extensionPreview(source),
  InstallExtension: ({ source = {}, expectedCommit, approvedPermissions = [] }) => {
    const p = extensionPreview(source);
    if (p.errors.length) return { errors: p.errors };
    if (p.commit !== expectedCommit) return connectError(400, "failed_precondition", "the ref moved since the preview; preview again");
    let e = extensions.find((x) => x.key === p.extensionKey);
    if (!e) { e = { id: `ext-${extensions.length + 1}`, key: p.extensionKey, repositoryUrl: source.repositoryUrl, gitRef: source.gitRef, createdAt: now(), versions: [], grants: [] }; extensions.push(e); }
    const version = { id: `extv-${extensions.reduce((n, x) => n + x.versions.length, 1)}`, extensionId: e.id, version: p.version, repositoryUrl: source.repositoryUrl, gitRef: source.gitRef, commit: p.commit, manifestJson: p.manifestJson,
      manifestSha256: p.manifestSha256, manifestOrigin: source.overlayManifestJson ? "overlay" : "repository", manifestPath: source.overlayManifestJson ? "" : source.manifestPath || "blaxsmith-extension.json",
      approvedPermissions: [...approvedPermissions].sort(), permissionsSha256: "c".repeat(64), installedByUsername: "you", createdAt: now(), permissions: p.permissions, templates: p.templates };
    e.versions.push(version); e.current = version.id; e.latestRefCommit = p.commit; auditEvent("workflow.extension.installed", version.id);
    return { extension: extensionSummary(e), version };
  },
  CheckExtensionUpdate: ({ extensionId }) => { const e = extensions.find((x) => x.id === extensionId); e.latestRefCommit = "9".repeat(40); e.checkedAt = now(); return { extension: extensionSummary(e) }; },
  GrantExtension: ({ extensionId, projectId: pid = "", granteeKind, granteeId = "" }) => { const grant = { id: `egrant-${Date.now()}`, projectId: pid, projectName: pid ? projectName(pid) : "", granteeKind, granteeId, createdAt: now() }; extensions.find((x) => x.id === extensionId)?.grants.push(grant); return { grant }; },
  RevokeExtensionGrant: ({ grantId }) => { for (const e of extensions) e.grants = e.grants.filter((g) => g.id !== grantId); return {}; },
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
  CreateClaudeSubscription: (body) => claudePolicy.allowed && /^sk-ant-oat[A-Za-z0-9_-]{8,500}$/.test(body?.setupToken ?? "")
    ? hubAdd({ scope: "personal", ownerId: principalId, kind: "subscription", provider: "anthropic", account: "claude-subscription", label: "Claude subscription", modelCount: 3, modelsCheckedAt: now() })
    : connectError(400, "failed_precondition", "your organization has not enabled members' own Claude subscriptions"),
  GetClaudeSubscriptionPolicy: () => ({ allowMemberClaudeSubscription: claudePolicy.allowed }),
  SetClaudeSubscriptionPolicy: (body) => { claudePolicy.allowed = Boolean(body?.allowMemberClaudeSubscription); return { allowMemberClaudeSubscription: claudePolicy.allowed }; },
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

// WorkspaceService (Home, Inbox, all runs, paged members) over the scenario above plus a
// few static projects and runs. MOCK_ROLE=member|viewer|admin|owner switches the session role.
const mockRole = process.env.MOCK_ROLE || "owner";
const mayAnswer = () => ["owner", "admin", "member"].includes(mockRole);
const mayDecide = () => ["owner", "admin"].includes(mockRole);
const projects = [
  { id: projectId, slug: "demo", name: "Demo project", createdAt: minutesAgo(60 * 24 * 21) },
  { id: "proj-billing", slug: "billing-service", name: "Billing service", createdAt: minutesAgo(60 * 24 * 14) },
  { id: "proj-mobile", slug: "mobile-app", name: "Mobile app", createdAt: minutesAgo(60 * 24 * 9) },
  { id: "proj-data", slug: "data-platform", name: "Data platform", createdAt: minutesAgo(60 * 24 * 3) },
];
const projectSources = new Map();
const projectChecks = new Map();
const projectName = (id) => projects.find((p) => p.id === id)?.name ?? id;
const sha = (n) => (n * 2654435761 >>> 0).toString(16).padStart(8, "0").repeat(5);
const staticRuns = [
  ["invoice-retry-fix", "proj-billing", "active", 190, "awaiting_input", 1, 2, 5],
  ["ledger-export-v2", "proj-billing", "succeeded", 60 * 5, "needs_approval", 0, 6, 6],
  ["tax-rounding", "proj-billing", "failed", 60 * 9, "failed", 0, 3, 5],
  ["push-opt-in", "proj-mobile", "active", 35, "working", 0, 1, 4],
  ["offline-cache", "proj-mobile", "succeeded", 60 * 30, "done", 0, 5, 5],
  ["dark-mode-audit", "proj-mobile", "cancelled", 60 * 50, "", 0, 2, 5],
  ["schema-drift-check", "proj-data", "queued", 4, "", 0, 0, 5],
  ["reindex-halt", "proj-data", "cancel_requested", 12, "", 0, 1, 5],
  ["backfill-partitions", "proj-data", "succeeded", 60 * 26, "done", 0, 4, 4],
  ["export-retention", projectId, "succeeded", 60 * 72, "done", 0, 6, 6],
  ["csv-header-fix", projectId, "failed", 60 * 96, "failed", 0, 2, 6],
  ...Array.from({ length: 14 }, (_, i) => [`nightly-refresh-${String(i + 1).padStart(2, "0")}`, "proj-data", i % 5 === 3 ? "failed" : "succeeded", 60 * (100 + i * 20), i % 5 === 3 ? "failed" : "done", 0, 4, 4]),
].map(([launchKey, pid, state, minutes, status, open, done, count], i) => ({ id: `run-${launchKey}`, projectId: pid, projectName: projectName(pid), launchKey, sourceCommit: sha(i + 3), state, createdAt: minutesAgo(minutes), status: status || stateStatus(state), openInteractions: open, reviewWaiting: status === "needs_approval" && state === "succeeded", stageCount: count, stagesSucceeded: done }));
// The server's rollup falls back to the run state when no stage says more.
function stateStatus(state) { return ["queued", "cancel_requested", "cancelled"].includes(state) ? state : ""; }
function liveRun() {
  const open = interactions.filter((i) => i.state === "open");
  const status = open.some((i) => i.kind === "approval") || (reviewPackage && !reviewPackage.decision) ? "needs_approval" : open.length ? "awaiting_input" : run.state === "active" ? "working" : run.state === "succeeded" ? "done" : stateStatus(run.state);
  return { ...run, projectName: projectName(projectId), status, openInteractions: open.length, reviewWaiting: Boolean(reviewPackage && !reviewPackage.decision), stageCount: tasks.length, stagesSucceeded: tasks.filter((t) => t.state === "succeeded").length };
}
const allRuns = () => [liveRun(), ...staticRuns];
const mockBudgetAlerts = []; // Filled by the model gateway G3 block below.
function inboxItems() {
  const items = interactions.filter((i) => i.state === "open").map((i) => ({ id: i.id, kind: i.kind, runId, projectId, projectName: projectName(projectId), runLaunchKey: run.launchKey, stage: i.stage, title: i.title, blocking: i.blocking, createdAt: i.createdAt, canAct: mayAnswer() }));
  if (reviewPackage && !reviewPackage.decision) items.push({ id: reviewPackage.id, kind: "review", runId, projectId, projectName: projectName(projectId), runLaunchKey: run.launchKey, stage: "", title: "Review package revision 1", blocking: true, createdAt: reviewPackage.presentedAt, canAct: mayDecide() });
  items.push(
    { id: "ix-billing-1", kind: "question", runId: "run-invoice-retry-fix", projectId: "proj-billing", projectName: "Billing service", runLaunchKey: "invoice-retry-fix", stage: "implement", title: "Should retries back off exponentially or on a fixed schedule?", blocking: true, createdAt: minutesAgo(52), canAct: mayAnswer() },
    { id: "pkg-ledger", kind: "review", runId: "run-ledger-export-v2", projectId: "proj-billing", projectName: "Billing service", runLaunchKey: "ledger-export-v2", stage: "", title: "Review package revision 2", blocking: true, createdAt: minutesAgo(60 * 4), canAct: mayDecide() },
  );
  if (mayDecide()) for (const a of mockBudgetAlerts.filter((x) => !x.acknowledgedAt && !(x.snoozedUntil && Date.parse(x.snoozedUntil) > Date.now()))) {
    items.push({ id: a.id, kind: "budget_alert", runId: "", projectId: a.projectId, projectName: a.projectName, runLaunchKey: "", stage: a.scope, title: `${a.budgetName} passed ${a.thresholdPct}% of its monthly budget`, blocking: false, createdAt: a.createdAt, canAct: true,
      target: a.scope === "project" ? "project_usage" : a.scope === "user" && a.principalId === principalId ? "my_usage" : "admin_alerts" });
  }
  return items.sort((a, b) => Number(b.blocking) - Number(a.blocking) || a.createdAt.localeCompare(b.createdAt));
}
const page = (rows, pageNumber = 1, size = 25) => rows.slice((Math.max(1, pageNumber) - 1) * size, Math.max(1, pageNumber) * size);
const members = [
  ["you", "You", "owner", "active", 2, 1], ["mara", "Mara Lindqvist", "admin", "active", 1, 30], ["dev.okafor", "Chidi Okafor", "member", "active", 3, 90],
  ["j.tanaka", "Jun Tanaka", "member", "active", 0, 60 * 26], ["priya", "Priya Raman", "member", "invited", 0, 0], ["sam.reviewer", "Sam Ortiz", "viewer", "active", 1, 60 * 8],
  ["ops-bot", "Operations", "admin", "active", 0, 60 * 24 * 6], ["l.chen", "Lena Chen", "member", "disabled", 0, 60 * 24 * 40], ["auditor", "External auditor", "viewer", "active", 0, 60 * 24 * 2],
  ...Array.from({ length: 16 }, (_, i) => [`eng${i + 1}`, `Engineer ${i + 1}`, i % 4 === 0 ? "viewer" : "member", i % 7 === 6 ? "invited" : "active", i % 3, 60 * (i + 2)]),
].map(([username, displayName, role, status, sessions, minutes], i) => ({ principalId: i === 0 ? principalId : `p-${username}`, username, displayName, role, status, activeSessions: sessions, lastLoginAt: status === "invited" ? "" : minutesAgo(minutes), createdAt: minutesAgo(60 * 24 * (30 - i)) }));
const roleOrder = ["owner", "admin", "member", "viewer"];
Object.assign(rpc, {
  CurrentSession: () => ({ session: { organizationId: "org-demo", principalId, role: mockRole, accessExpiresAt: new Date(Date.now() + 3_600_000).toISOString() } }),
  GetProject: ({ projectId: pid = projectId }) => { const p = projects.find((x) => x.id === pid); return p ? { project: p } : connectError(404, "not_found", "workflow resource not found"); },
  // New projects start with no source or checks, so the setup flow's repository prefill shows.
  CreateProject: ({ slug, name }) => {
    if (projects.some((p) => p.slug === slug)) return connectError(409, "already_exists", "project slug already exists");
    const p = { id: `proj-${slug}`, slug, name, createdAt: now() };
    projects.push(p);
    return { project: p };
  },
  GetProjectSource: ({ projectId: pid = projectId }) => pid === projectId ? { source: { projectId, repositoryUrl: "https://github.com/example/blaxsmith-demo.git", ref: "main" } } : projectSources.has(pid) ? { source: projectSources.get(pid) } : {},
  SetProjectSource: ({ projectId: pid, repositoryUrl, ref = "", gitConnectionId = "" }) => { const s = { projectId: pid, repositoryUrl, ref, gitConnectionId }; projectSources.set(pid, s); return { source: s }; },
  GetProjectVerification: ({ projectId: pid = projectId }) => pid === projectId ? { verification: { projectId, version: "1", checks: [{ id: "go-test", command: ["go", "test", "./..."] }] } } : projectChecks.has(pid) ? { verification: projectChecks.get(pid) } : {},
  SetProjectVerification: ({ projectId: pid, checks }) => { const v = { projectId: pid, version: "1", checks, updatedAt: now() }; projectChecks.set(pid, v); return { verification: v }; },
  ListProjects: ({ search = "", sortBy = "created_at", sortDirection = "desc" }) => {
    const rows = projects.filter((p) => `${p.name} ${p.slug}`.toLowerCase().includes(search.toLowerCase()))
      .sort((a, b) => (sortBy === "name" ? a.name.localeCompare(b.name) : a.createdAt.localeCompare(b.createdAt)) * (sortDirection === "asc" ? 1 : -1));
    return { projects: rows, nextPageToken: "" };
  },
  GetWorkspaceHome: () => {
    const items = inboxItems();
    const actionable = items.filter((i) => i.canAct);
    const runsNow = allRuns();
    const agents = rpc.GetAdminOverview().liveAttempts.map((a) => ({ ...a, takenOver: Boolean(a.controllerPrincipalId) }));
    return { organizationName: "Acme Engineering", organizationSlug: "acme", username: "you", displayName: members[0].displayName, email: members[0].email,
      waitingOnYou: actionable.length, openItems: items.length, runningAgents: agents.length, activeRuns: runsNow.filter((r) => ["queued", "active", "cancel_requested"].includes(r.state)).length,
      runsLast24h: runsNow.filter((r) => Date.now() - Date.parse(r.createdAt) < 86_400_000).length, failedLast24h: runsNow.filter((r) => r.state === "failed" && Date.now() - Date.parse(r.createdAt) < 86_400_000).length,
      waiting: actionable.slice(0, 5), agents: agents.slice(0, 10), recentRuns: [...runsNow].sort((a, b) => b.createdAt.localeCompare(a.createdAt)).slice(0, 8), generatedAt: now() };
  },
  ListInbox: ({ page: p = 1, pageSize = 25, kinds = [], projectId: pid = "", search = "", actionableOnly = false }) => {
    const rows = inboxItems().filter((i) => (!actionableOnly || i.canAct) && (!kinds.length || kinds.includes(i.kind)) && (!pid || i.projectId === pid)
      && (!search || `${i.title} ${i.projectName} ${i.runLaunchKey} ${i.stage}`.toLowerCase().includes(search.toLowerCase())));
    return { items: page(rows, p, pageSize), totalCount: rows.length };
  },
  ListWorkspaceRuns: ({ page: p = 1, pageSize = 25, search = "", states = [], projectId: pid = "", sortBy = "created_at", sortDirection = "desc" }) => {
    const key = { launch_key: "launchKey", project: "projectName", state: "state", created_at: "createdAt" }[sortBy] ?? "createdAt";
    const rows = allRuns().filter((r) => (!pid || r.projectId === pid) && (!states.length || states.includes(r.state))
      && (!search || `${r.launchKey} ${r.sourceCommit} ${r.projectName}`.toLowerCase().includes(search.toLowerCase())))
      .sort((a, b) => String(a[key]).localeCompare(String(b[key])) * (sortDirection === "asc" ? 1 : -1));
    return { runs: page(rows, p, pageSize), totalCount: rows.length };
  },
  ListOrgMembers: () => mayDecide() ? { members } : connectError(403, "permission_denied", "organization administration denied"),
  ListMembersPage: ({ page: p = 1, pageSize = 25, search = "", roles = [], statuses = [], sortBy = "role", sortDirection = "asc" }) => {
    if (!mayDecide()) return connectError(403, "permission_denied", "organization administration denied");
    const cmp = { role: (a, b) => roleOrder.indexOf(a.role) - roleOrder.indexOf(b.role), email: (a, b) => (a.email || a.username).localeCompare(b.email || b.username),
      last_login: (a, b) => a.lastLoginAt.localeCompare(b.lastLoginAt), created_at: (a, b) => a.createdAt.localeCompare(b.createdAt) }[sortBy] ?? (() => 0);
    const rows = members.filter((m) => (!search || `${m.email} ${m.username} ${m.displayName}`.toLowerCase().includes(search.toLowerCase())) && (!roles.length || roles.includes(m.role)) && (!statuses.length || statuses.includes(m.status)))
      .sort((a, b) => cmp(a, b) * (sortDirection === "desc" ? -1 : 1) || a.username.localeCompare(b.username));
    return { members: page(rows, p, pageSize), totalCount: rows.length };
  },
  SetUserEnabled: ({ principalId: id, enabled }) => { const m = members.find((x) => x.principalId === id); if (!m) return connectError(404, "not_found", "no such member"); if (id === principalId) return connectError(400, "failed_precondition", "you cannot disable yourself"); m.status = enabled ? "active" : "disabled"; if (!enabled) m.activeSessions = 0; auditEvent(enabled ? "identity.user.enabled" : "identity.user.disabled", id); return {}; },
  SetUserRole: ({ principalId: id, role }) => { const m = members.find((x) => x.principalId === id); if (m) { m.role = role; m.activeSessions = 0; } auditEvent("identity.user.role_changed", id); return {}; },
  RevokeUserSessions: ({ principalId: id }) => { const m = members.find((x) => x.principalId === id); const revoked = m?.activeSessions ?? 0; if (m) m.activeSessions = 0; auditEvent("identity.user.sessions_revoked", id); return { revoked: String(revoked) }; },
  IssueResetLink: ({ principalId: id }) => { auditEvent("identity.user.reset_link_issued", id); return { link: { token: "mock-reset-token", purpose: "reset", expiresAt: new Date(Date.now() + 86_400_000).toISOString() } }; },
  InviteUser: ({ email = "", displayName = "", role = "member" }) => {
    email = email.trim().toLowerCase();
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return connectError(400, "invalid_argument", "enter a valid email address");
    if (members.some((m) => m.email === email)) return connectError(409, "already_exists", "that email is already in use");
    const username = email.split("@")[0].replace(/[^a-z0-9._-]/g, "-");
    members.push({ principalId: `p-${username}`, username, email, emailVerified: false, displayName, role, status: "invited", activeSessions: 0, lastLoginAt: "", createdAt: now() });
    auditEvent("identity.user.invited", `p-${username}`);
    return { principalId: `p-${username}`, link: { token: "mock-setup-token", purpose: "setup", expiresAt: new Date(Date.now() + 86_400_000).toISOString() } };
  },
  SetUserEmail: ({ principalId: id, email = "" }) => {
    email = email.trim().toLowerCase();
    const m = members.find((x) => x.principalId === id);
    if (!m) return connectError(404, "not_found", "member not found");
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) return connectError(400, "invalid_argument", "enter a valid email address");
    if (members.some((x) => x.email === email && x !== m)) return connectError(409, "already_exists", "that email is already in use");
    Object.assign(m, { email, emailVerified: false, activeSessions: 0 });
    auditEvent("identity.user.email_changed", id);
    return {};
  },
  GetAccountLink: ({ token }) => token.startsWith("mock-") ? { purpose: token.includes("reset") ? "reset" : "setup", username: "priya", email: token.includes("legacy") ? "" : "priya@acme.test",
    displayName: "Priya Raman", organizationSlug: "acme", organizationName: "Acme Engineering", expiresAt: new Date(Date.now() + 86_400_000).toISOString() } : connectError(404, "not_found", "account link is invalid, used, or expired"),
  CompleteAccountLink: ({ password = "", email = "" }) => new TextEncoder().encode(password).length < 12 ? connectError(400, "invalid_argument", "password must be valid UTF-8 and 12–1024 bytes")
    : { organizationSlug: "acme", email: email || "priya@acme.test" },
});
members.forEach((m) => Object.assign(m, { email: m.status === "invited" && m.username !== "priya" ? "" : `${m.username}@acme.test`, emailVerified: m.username === "you" }));

// Sign-in and the caller's own account; the mock password is "mock password
// 123". MOCK_SIGNED_OUT=1 starts at the login page (sign in as you@acme.test).
// MOCK_LEGACY=1 starts signed out with no email on the account, so the
// username "you" works once and leads to the set-email page;
// MOCK_EMAIL_REQUIRED=1 starts in that one-time session.
const mockPassword = "mock password 123";
const me = members[0];
const legacyAccount = process.env.MOCK_LEGACY === "1" || process.env.MOCK_EMAIL_REQUIRED === "1";
if (legacyAccount) me.email = "";
let signedIn = process.env.MOCK_SIGNED_OUT !== "1" && process.env.MOCK_LEGACY !== "1";
let emailRequired = process.env.MOCK_EMAIL_REQUIRED === "1";
let legacyUsed = emailRequired;
let myPassword = mockPassword;
const unauthenticated = () => connectError(401, "unauthenticated", "authentication required");
const mySessions = [
  { id: "s-current", current: true, organizationSlug: "acme", createdAt: minutesAgo(90), lastSeenAt: minutesAgo(2), expiresAt: new Date(Date.now() + 6 * 86_400_000).toISOString(), sourceAddress: "127.0.0.1", userAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 14_6) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Safari/605.1.15" },
  { id: "s-laptop", current: false, organizationSlug: "acme", createdAt: minutesAgo(60 * 26), lastSeenAt: minutesAgo(60 * 3), expiresAt: new Date(Date.now() + 5 * 86_400_000).toISOString(), sourceAddress: "203.0.113.24", userAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0 Safari/537.36" },
  { id: "s-phone", current: false, organizationSlug: "acme", createdAt: minutesAgo(60 * 50), lastSeenAt: "", expiresAt: new Date(Date.now() + 4 * 86_400_000).toISOString(), sourceAddress: "198.51.100.7", userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.0 Mobile/15E148 Safari/604.1" },
];
const myProfile = () => ({ principalId, email: me.email, emailVerified: me.emailVerified, displayName: me.displayName, handle: me.username,
  organizationId: "org-demo", organizationSlug: "acme", organizationName: "Acme Engineering", role: mockRole, createdAt: me.createdAt, emailRequired });
const baseSession = rpc.CurrentSession;
// Requests (not the startup seeding below) need a session, and an
// email_required session reaches only the account-setup calls, as on the server.
const publicMethods = new Set(["GetCsrf", "CurrentSession", "RefreshSession", "LoginLocal", "Logout", "GetAccountLink", "CompleteAccountLink", "ListTools"]);
const setupMethods = new Set(["GetMyProfile", "UpdateMyProfile"]);
const refuse = (method) => publicMethods.has(method) ? null : !signedIn ? unauthenticated()
  : emailRequired && !setupMethods.has(method) ? connectError(400, "failed_precondition", "set your email to continue") : null;
Object.assign(rpc, {
  CurrentSession: () => signedIn ? { session: { ...baseSession().session, emailRequired } } : unauthenticated(),
  RefreshSession: () => rpc.CurrentSession(),
  LoginLocal: ({ email = "", username = "", password = "" }) => {
    const login = (email || username).trim().toLowerCase();
    const legacy = !login.includes("@");
    const known = legacy ? !legacyUsed && !me.email && login === me.username : Boolean(me.email) && login === me.email;
    if (password !== myPassword || !known) return unauthenticated();
    if (legacy) { legacyUsed = true; emailRequired = true; }
    signedIn = true;
    return rpc.CurrentSession();
  },
  Logout: () => { signedIn = false; return {}; },
  GetMyProfile: () => signedIn ? { profile: myProfile() } : unauthenticated(),
  UpdateMyProfile: ({ displayName, email, currentPassword = "" }) => {
    if (!signedIn) return unauthenticated();
    if (emailRequired && email === undefined) return connectError(400, "failed_precondition", "set your email to continue");
    let revokedSessions = "0";
    if (email !== undefined && (emailRequired || email.trim().toLowerCase() !== me.email)) {
      const next = email.trim().toLowerCase();
      if (currentPassword !== myPassword) return connectError(403, "permission_denied", "current password is incorrect");
      if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(next)) return connectError(400, "invalid_argument", "enter a valid email address");
      if (members.some((m) => m.email === next && m !== me)) return connectError(409, "already_exists", "that email is already in use");
      Object.assign(me, { email: next, emailVerified: false });
      emailRequired = false;
      revokedSessions = String(mySessions.filter((s) => !s.current).length);
      mySessions.splice(0, mySessions.length, ...mySessions.filter((s) => s.current));
      auditEvent("identity.account.email_changed", principalId);
    }
    if (displayName !== undefined && displayName.trim() !== me.displayName) {
      if (displayName.trim().length > 160) return connectError(400, "invalid_argument", "invalid user input");
      me.displayName = displayName.trim();
      auditEvent("identity.account.profile_updated", principalId);
    }
    return { profile: myProfile(), revokedSessions };
  },
  ChangeMyPassword: ({ currentPassword = "", newPassword = "" }) => {
    if (currentPassword !== myPassword) return connectError(403, "permission_denied", "current password is incorrect");
    const bytes = new TextEncoder().encode(newPassword).length;
    if (bytes < 12 || bytes > 1024) return connectError(400, "invalid_argument", "password must be valid UTF-8 and 12–1024 bytes");
    myPassword = newPassword;
    const revokedSessions = String(mySessions.filter((s) => !s.current).length);
    mySessions.splice(0, mySessions.length, ...mySessions.filter((s) => s.current));
    auditEvent("identity.account.password_changed", principalId);
    return { revokedSessions };
  },
  ListMySessions: () => ({ sessions: mySessions }),
  RevokeMySession: ({ sessionId }) => {
    const index = mySessions.findIndex((s) => s.id === sessionId);
    if (index < 0) return connectError(404, "not_found", "session not found");
    if (mySessions[index].current) return connectError(400, "failed_precondition", "use sign out to end this session");
    mySessions.splice(index, 1);
    auditEvent("identity.account.session_revoked", sessionId);
    return {};
  },
  RevokeMyOtherSessions: () => {
    const revoked = String(mySessions.filter((s) => !s.current).length);
    mySessions.splice(0, mySessions.length, ...mySessions.filter((s) => s.current));
    auditEvent("identity.account.other_sessions_revoked", principalId);
    return { revoked };
  },
});
// Seed the connections hub so connection lists and detail tabs have content.
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "Acme Engineering", kind: "api_key", provider: "anthropic", label: "Platform team", account: "sk-ant-…a41f", modelCount: 6, modelsCheckedAt: minutesAgo(40), lastUsedAt: minutesAgo(3), createdAt: minutesAgo(60 * 24 * 12),
  grants: [{ id: "cg-1", projectId, projectName: "Demo project", granteeKind: "project", granteeId: "", granteeName: "", createdAt: minutesAgo(60 * 24 * 10) }, { id: "cg-2", projectId: "", projectName: "", granteeKind: "role", granteeId: "member", granteeName: "", createdAt: minutesAgo(60 * 24 * 8) }],
  uses: [{ id: "cu-1", projectId, projectName: "Demo project", model: "claude-opus-5-5", granteeKind: "workload", createdAt: minutesAgo(60 * 24 * 9) }] });
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "Acme Engineering", kind: "git", provider: "github", label: "", account: "acme-bot", lastUsedAt: minutesAgo(12), createdAt: minutesAgo(60 * 24 * 20) });
hubAdd({ scope: "organization", ownerId: "org-demo", ownerName: "Acme Engineering", kind: "api_key", provider: "openai", label: "Evaluation", account: "sk-…9c2e", modelCount: 0, modelsCheckedAt: minutesAgo(15), modelsError: "The provider rejected the key (401).", state: "reconnect_required", createdAt: minutesAgo(60 * 24 * 2) });
hubAdd({ scope: "personal", ownerId: principalId, ownerName: "You", kind: "subscription", provider: "codex", label: "", account: "acct-demo", modelCount: 3, modelsCheckedAt: minutesAgo(90), lastUsedAt: minutesAgo(200), createdAt: minutesAgo(60 * 24 * 5),
  uses: [{ id: "cu-2", projectId, projectName: "Demo project", model: "gpt-5.6-luna", granteeKind: "user", createdAt: minutesAgo(60 * 24 * 4) }] });
hubAdd({ scope: "project", ownerId: projectId, ownerName: "Demo project", kind: "api_key", provider: "anthropic", label: "Project key", account: "sk-ant-…77b0", modelCount: 4, modelsCheckedAt: minutesAgo(300), createdAt: minutesAgo(60 * 24 * 6) });

// Seeded extensions: Guild granted to the demo project (two versions, the ref
// moved since), and a lint pack installed but granted nowhere.
{
  const guild = { repositoryUrl: "https://github.com/example/guild", gitRef: "main", manifestPath: "", overlayManifestJson: "" };
  const older = JSON.parse(guildManifest);
  older.version = "0.9.0";
  const all = (src) => extensionPreview(src).permissions.map((p) => p.id);
  rpc.InstallExtension({ source: { ...guild, overlayManifestJson: JSON.stringify(older) }, expectedCommit: extensionPreview({ overlayManifestJson: JSON.stringify(older) }).commit, approvedPermissions: all({ overlayManifestJson: JSON.stringify(older) }) });
  rpc.InstallExtension({ source: guild, expectedCommit: extensionPreview(guild).commit, approvedPermissions: extensionPreview(guild).permissions.filter((p) => !p.optional).map((p) => p.id) });
  const g = extensions.find((e) => e.key === "guild");
  g.versions[0].createdAt = minutesAgo(60 * 24 * 12); g.versions[1].createdAt = minutesAgo(60 * 24 * 2); g.createdAt = g.versions[0].createdAt;
  g.latestRefCommit = "7c2d".padEnd(40, "1"); g.checkedAt = minutesAgo(90);
  rpc.GrantExtension({ extensionId: g.id, projectId, granteeKind: "project" });
  const lint = { id: "lint-pack", version: "2.1.0", title: "Lint pack", native_subagents: false, mcp_servers: [], hooks: [], egress: [], runtimes: [],
    stage_templates: [{ id: "strict-review", title: "Strict review", mode: "embedded", harness: "claude-code", kinds: ["review"] }] };
  const lintSource = { repositoryUrl: "https://gitlab.com/example/lint-pack", gitRef: "v2", manifestPath: "", overlayManifestJson: JSON.stringify(lint) };
  rpc.InstallExtension({ source: lintSource, expectedCommit: extensionPreview(lintSource).commit, approvedPermissions: [] });
  extensions.find((e) => e.key === "lint-pack").checkedAt = minutesAgo(60 * 5);
}
function mayUseExtension(e, pid) {
  const rank = (role) => ["member", "admin", "owner"].indexOf(role);
  return mockRole !== "viewer" && e.grants.some((g) => (g.granteeKind === "project" && g.projectId === pid) || (g.granteeKind === "user" && g.granteeId === principalId)
    || (g.granteeKind === "role" && rank(mockRole) >= rank(g.granteeId)));
}

// Model gateway (G1): settings, usage and run cost. MOCK_GATEWAY=off starts
// with the master switch off, so the gated UI can be checked hidden.
{
  const gw = { enabled: process.env.MOCK_GATEWAY !== "off", defaultDeliveryMode: "brokered_gateway", allowProjectChoice: true, removeDirectEgress: true, version: 3, updatedAt: minutesAgo(60 * 20) };
  let projectChoice = "";
  const overrides = [];
  const projects = [["proj-demo", "Demo project"], ["proj-billing", "Billing service"], ["proj-search", "Search indexer"], ["proj-docs", "Docs site"], ["proj-mobile", "Mobile app"], ["proj-infra", "Infra scripts"], ["proj-ml", "ML pipeline"]];
  const models = ["claude-opus-5-5", "gpt-6-luna", "claude-sonnet-5", "kimi-k3"];
  const totals = (cost, requests = Math.round(cost / 40_000) + 1) => ({ requests: String(requests), errors: String(Math.floor(requests / 40)), rateLimited: String(Math.floor(requests / 90)),
    inputTokens: String(cost * 2), outputTokens: String(Math.round(cost / 3)), cacheReadTokens: String(cost * 5), cacheWriteTokens: String(Math.round(cost / 2)), reasoningTokens: String(Math.round(cost / 9)), costUsdMicros: String(cost) });
  const today = new Date(); today.setUTCHours(0, 0, 0, 0);
  const day = (back) => new Date(today.getTime() - back * 86_400_000).toISOString().slice(0, 10);
  const wave = (i, k) => Math.round((Math.sin(i / 3 + k) + 1.4) * (900_000 - k * 110_000) + (i % 7 === 5 || i % 7 === 6 ? -200_000 : 0));
  const priceRows = () => [
    ["anthropic", "claude-opus-5-5", 4, 20, 0.2, 5], ["anthropic", "claude-sonnet-5", 2, 10, 0.2, 2.5], ["anthropic", "claude-haiku-4-5", 1, 5, 0.1, 1.25],
  ].map(([provider, model, i, o, r, w]) => ({ provider, model, inputMicrosPerMtok: String(i * 1e6), outputMicrosPerMtok: String(o * 1e6), cacheReadMicrosPerMtok: String(r * 1e6),
    cacheWriteMicrosPerMtok: String(w * 1e6), source: "manifest", version: "manifest:2026-09-24", effectiveFrom: "" }))
    .map((p) => overrides.find((o) => o.provider === p.provider && o.model === p.model) ?? p).concat(overrides.filter((o) => !["claude-opus-5-5", "claude-sonnet-5", "claude-haiku-4-5"].includes(o.model)));
  const delivery = () => ({ deliveryMode: !gw.enabled ? "native_raw" : gw.allowProjectChoice && projectChoice ? projectChoice : gw.defaultDeliveryMode,
    projectChoice, orgDefault: gw.defaultDeliveryMode, gatewayEnabled: gw.enabled, choiceAllowed: gw.allowProjectChoice, canEdit: true });
  const stageCosts = { plan: 1_840_000, implement: 3_120_000, verify: 960_000, review: 410_000 };
  Object.assign(rpc, {
    GetGatewayStatus: () => ({ enabled: gw.enabled }),
    GetGatewaySettings: () => ({ settings: { enabled: gw.enabled, defaultDeliveryMode: gw.defaultDeliveryMode, allowProjectChoice: gw.allowProjectChoice, removeDirectEgress: gw.removeDirectEgress },
      installationAvailable: true, version: String(gw.version), updatedAt: gw.updatedAt, updatedByUsername: "you" }),
    UpdateGatewaySettings: ({ settings, expectedVersion }) => {
      if (Number(expectedVersion) !== gw.version) return connectError(409, "aborted", "settings changed since you loaded them");
      Object.assign(gw, { enabled: false, allowProjectChoice: false, removeDirectEgress: false }, settings, { version: gw.version + 1, updatedAt: now() }); auditEvent("gateway.settings.updated", "org-demo");
      return { settings, version: String(gw.version) };
    },
    GetUsageOverview: ({ days = 30, seriesBy = "project" }) => {
      const keys = seriesBy === "model" ? models.map((m) => [m, m]) : projects;
      const series = [];
      for (let i = days - 1; i >= 0; i--) keys.forEach(([key], k) => { const cost = Math.max(0, wave(i, k)); if (cost) series.push({ day: day(i), key, costUsdMicros: String(cost), tokens: String(cost * 8) }); });
      const sum = (key) => series.filter((p) => p.key === key).reduce((n, p) => n + Number(p.costUsdMicros), 0);
      const all = series.reduce((n, p) => n + Number(p.costUsdMicros), 0);
      const top = projects.map(([key, label]) => ({ key, label, detail: "", totals: totals(seriesBy === "project" ? sum(key) : Math.round(all / projects.length)) }));
      return { enabled: gw.enabled, fromDay: day(days - 1), toDay: day(0), totals: totals(all), medianTtftMs: "840", series,
        seriesLabels: keys.map(([key, label]) => ({ key, label, detail: "", totals: totals(sum(key)) })),
        topProjects: top.sort((a, b) => Number(b.totals.costUsdMicros) - Number(a.totals.costUsdMicros)),
        topUsers: [["p-you", "You", "you"], ["p-ana", "Ana Ruiz", "ana"], ["p-sam", "Sam Lee", "sam"]].map(([key, label, detail], k) => ({ key, label, detail, totals: totals(Math.round(all / (k + 2))) })),
        topRuns: [{ key: runId, label: run.launchKey, detail: "Demo project", projectId, totals: totals(6_330_000) }, { key: "run-stuck", label: "invoice-retry-fix", detail: "Billing service", projectId: "proj-billing", totals: totals(2_410_000) }] };
    },
    ListModelPrices: () => ({ prices: priceRows() }),
    SetModelPriceOverride: ({ price }) => { const p = { ...price, source: "override", version: `override:${overrides.length + 1}`, effectiveFrom: now() }; overrides.splice(overrides.findIndex((o) => o.model === p.model) >>> 0, 1); overrides.push(p); auditEvent("gateway.price.overridden", "org-demo"); return { price: p }; },
    GetMyUsage: ({ days = 30 }) => {
      const scale = days / 30;
      const byProject = projects.slice(0, 3).map(([key, label], k) => ({ key, label, detail: "", totals: totals(Math.round((4_100_000 - k * 1_100_000) * scale)) }));
      const byModel = models.slice(0, 3).map((m, k) => ({ key: m, label: m, detail: k === 1 ? "openai" : "anthropic", totals: totals(Math.round((5_200_000 - k * 1_700_000) * scale)) }));
      return { enabled: gw.enabled, fromDay: day(days - 1), toDay: day(0), totals: totals(Math.round(8_800_000 * scale)), byProject, byModel,
        topRuns: [{ key: runId, label: run.launchKey, detail: "Demo project", projectId, totals: totals(6_330_000) }] };
    },
    GetRunCost: () => {
      const stages = Object.entries(stageCosts).map(([stage, cost]) => ({ taskId: `t-${stage}`, stage, totals: totals(cost), cacheHitRatio: stage === "implement" ? 0.71 : 0.48 }));
      const requests = [];
      for (let i = 0; i < 24; i++) {
        const stage = Object.keys(stageCosts)[i % 4];
        const limited = i === 7;
        requests.push({ startedAt: new Date(Date.now() - i * 95_000).toISOString(), stage, model: stage === "implement" ? "gpt-6-luna" : "claude-opus-5-5", routeKind: stage === "implement" ? "openai" : "anthropic",
          api: stage === "implement" ? "openai_responses" : "anthropic_messages", status: limited ? "error" : "ok", httpStatus: limited ? 429 : 200, retryCount: 0, streamed: true, usageReported: !limited,
          ttftMs: limited ? -1 : 600 + (i * 37) % 900, durationMs: 4_000 + (i * 911) % 30_000, totals: limited ? totals(0, 1) : totals(120_000 + (i * 7_919) % 300_000, 1) });
      }
      const all = Object.values(stageCosts).reduce((a, b) => a + b, 0);
      return { enabled: gw.enabled, totals: totals(all, 24), stages, requests, truncated: false };
    },
    GetProjectDelivery: () => delivery(),
    SetProjectDelivery: ({ deliveryMode = "" }) => { projectChoice = deliveryMode; auditEvent("gateway.project_delivery.updated", projectId); return { delivery: delivery() }; },
  });
}

// Model gateway G3: budgets, alerts and Project → Usage. Alerts also show in
// the inbox (kind budget_alert) for owners and admins.
{
  const month = new Date(); month.setUTCDate(1); month.setUTCHours(0, 0, 0, 0);
  const monthEnd = new Date(Date.UTC(month.getUTCFullYear(), month.getUTCMonth() + 1, 0));
  const elapsed = Math.max(1, (Date.now() - month.getTime()) / 86_400_000);
  const days = monthEnd.getUTCDate();
  const forecast = (spend) => String(Math.round(spend * days / elapsed));
  let budgetsEnabled = true;
  let version = 0;
  const budgets = [
    ["Acme engineering", "organization", "", "", 900_000_000, 612_000_000],
    ["Billing service", "project", "proj-billing", "", 150_000_000, 131_500_000],
    ["Demo project", "project", projectId, "", 120_000_000, 38_200_000],
    ["Chidi's experiments", "user", "", "p-dev.okafor", 40_000_000, 41_300_000],
  ].map(([name, scope, pid, principal, amount, spend], i) => ({ id: `budget-${i + 1}`, name, scope, projectId: pid, projectName: pid ? projectName(pid) : "",
    principalId: principal, principalName: principal ? "Chidi Okafor" : "", amountUsdMicros: String(amount), thresholds: [50, 80, 100], version: "1",
    spendUsdMicros: String(spend), forecastUsdMicros: forecast(spend), firedThresholds: [50, 80, 100].filter((t) => spend * 100 >= amount * t),
    periodStart: month.toISOString().slice(0, 10), periodEnd: monthEnd.toISOString().slice(0, 10), createdAt: minutesAgo(60 * 24 * 20) }));
  const alertFor = (b, t, minutes, ack) => ({ id: `alert-${b.id}-${t}`, budgetId: b.id, budgetName: b.name, scope: b.scope, projectId: b.projectId, projectName: b.projectName,
    principalId: b.principalId, principalName: b.principalName, thresholdPct: t, spendUsdMicros: String(Math.round(Number(b.amountUsdMicros) * t / 100 + 400_000)),
    amountUsdMicros: b.amountUsdMicros, forecastUsdMicros: b.forecastUsdMicros, periodStart: b.periodStart, createdAt: minutesAgo(minutes),
    acknowledgedAt: ack ? minutesAgo(minutes - 30) : "", acknowledgedByUsername: ack ? "mara" : "", snoozedUntil: "" });
  mockBudgetAlerts.push(alertFor(budgets[3], 100, 90, false), alertFor(budgets[1], 80, 60 * 5, false), alertFor(budgets[0], 50, 60 * 30, true),
    alertFor(budgets[1], 50, 60 * 48, true), alertFor(budgets[3], 80, 60 * 50, true));
  const active = () => budgets.filter((b) => !b.archived);
  Object.assign(rpc, {
    ListBudgets: () => mayDecide() ? { gatewayEnabled: process.env.MOCK_GATEWAY !== "off", budgetsEnabled, budgets: active() } : connectError(403, "permission_denied", "organization administration denied"),
    SetBudgetsEnabled: ({ enabled = false }) => { budgetsEnabled = enabled; auditEvent("gateway.budgets.settings.updated", "org-demo"); return { enabled }; },
    CreateBudget: ({ budget: b = {} }) => {
      if (active().some((x) => x.scope === b.scope && x.projectId === (b.projectId ?? "") && x.principalId === (b.principalId ?? ""))) return connectError(409, "already_exists", "an active budget already exists for this scope");
      const member = members.find((m) => m.principalId === b.principalId);
      const row = { id: `budget-new-${++version}`, name: b.name, scope: b.scope, projectId: b.projectId ?? "", projectName: b.projectId ? projectName(b.projectId) : "",
        principalId: b.principalId ?? "", principalName: member?.displayName ?? "", amountUsdMicros: String(b.amountUsdMicros), thresholds: b.thresholds?.length ? b.thresholds : [50, 80, 100],
        version: "1", spendUsdMicros: "0", forecastUsdMicros: "0", firedThresholds: [], periodStart: budgets[0].periodStart, periodEnd: budgets[0].periodEnd, createdAt: now() };
      budgets.push(row); auditEvent("gateway.budget.created", row.id); return { budget: row };
    },
    UpdateBudget: ({ budgetId, budget: b = {}, expectedVersion }) => {
      const row = active().find((x) => x.id === budgetId);
      if (!row) return connectError(404, "not_found", "workflow resource not found");
      if (String(expectedVersion) !== row.version) return connectError(409, "aborted", "the budget changed since you loaded it; reload and try again");
      Object.assign(row, { name: b.name, amountUsdMicros: String(b.amountUsdMicros), thresholds: b.thresholds, version: String(Number(row.version) + 1) });
      auditEvent("gateway.budget.updated", row.id); return { budget: row };
    },
    ArchiveBudget: ({ budgetId }) => { const row = active().find((x) => x.id === budgetId); if (row) row.archived = true; auditEvent("gateway.budget.archived", budgetId); return {}; },
    ListBudgetAlerts: ({ openOnly = false }) => mayDecide() ? { alerts: mockBudgetAlerts.filter((a) => !openOnly || !a.acknowledgedAt) } : connectError(403, "permission_denied", "organization administration denied"),
    AcknowledgeBudgetAlert: ({ alertId }) => { const a = mockBudgetAlerts.find((x) => x.id === alertId); if (!a) return connectError(404, "not_found", "workflow resource not found");
      if (!a.acknowledgedAt) Object.assign(a, { acknowledgedAt: now(), acknowledgedByUsername: "you" }); auditEvent("gateway.budget_alert.acknowledged", alertId); return {}; },
    SnoozeBudgetAlert: ({ alertId, hours = 24 }) => { const a = mockBudgetAlerts.find((x) => x.id === alertId); if (!a) return connectError(404, "not_found", "workflow resource not found");
      a.snoozedUntil = new Date(Date.now() + hours * 3_600_000).toISOString(); auditEvent("gateway.budget_alert.snoozed", alertId); return { snoozedUntil: a.snoozedUntil }; },
    GetProjectUsage: ({ projectId: pid = projectId }) => {
      const budget = active().find((b) => b.scope === "project" && b.projectId === pid);
      const spend = Number(budget?.spendUsdMicros ?? 21_400_000);
      const series = [];
      for (let d = new Date(month); d <= new Date(); d = new Date(d.getTime() + 86_400_000)) ["claude-opus-5-5", "gpt-6-luna"].forEach((model, k) =>
        series.push({ day: d.toISOString().slice(0, 10), key: model, costUsdMicros: String(Math.round(spend / elapsed * (k ? 0.35 : 0.65) * (0.7 + ((d.getUTCDate() * 7 + k) % 5) / 8))), tokens: "0" }));
      const t = (share) => ({ requests: String(Math.round(share * 900)), errors: String(Math.round(share * 12)), rateLimited: "0", inputTokens: String(Math.round(share * 4e6)), outputTokens: String(Math.round(share * 6e5)),
        cacheReadTokens: String(Math.round(share * 9e6)), cacheWriteTokens: "0", reasoningTokens: "0", costUsdMicros: String(Math.round(spend * share)) });
      return { enabled: process.env.MOCK_GATEWAY !== "off", fromDay: month.toISOString().slice(0, 10), toDay: new Date().toISOString().slice(0, 10), totals: t(1), budget,
        series, byStage: [["implement", 0.52], ["plan", 0.24], ["verify", 0.16], ["review", 0.08]].map(([key, s]) => ({ key, label: key, detail: "", totals: t(s) })),
        byModel: [["claude-opus-5-5", 0.65, "anthropic"], ["gpt-6-luna", 0.35, "openai"]].map(([key, s, detail]) => ({ key, label: key, detail, totals: t(s) })),
        byUser: mayDecide() ? [["p-you", "You", "you", 0.6], ["p-dev.okafor", "Chidi Okafor", "dev.okafor", 0.4]].map(([key, label, detail, s]) => ({ key, label, detail, totals: t(s) })) : [],
        byUserHidden: !mayDecide(),
        topRuns: [{ key: runId, label: run.launchKey, detail: projectName(pid), projectId: pid, totals: t(0.31) }, { key: "run-export-retention", label: "export-retention", detail: projectName(pid), projectId: pid, totals: t(0.12) }],
        alerts: mockBudgetAlerts.filter((a) => a.scope === "project" && a.projectId === pid) };
    },
  });
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
  const refused = handler ? refuse(method) : null;
  const result = refused ? refused : handler ? handler(body ? JSON.parse(body) : {}) : connectError(404, "unimplemented", `mock has no ${method ?? url.pathname}`);
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
  console.log(`[mock] API on http://127.0.0.1:${apiPort} · open http://127.0.0.1:${webPort}/projects/${projectId}/runs/${runId} or /admin (role ${process.env.MOCK_ROLE || "owner"})`);
});
