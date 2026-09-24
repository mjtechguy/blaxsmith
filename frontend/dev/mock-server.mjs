// Dev-only mock of the Blaxsmith API for exercising the run page (not bundled).
//   npm run dev:mock   → mock API on :8001 + Vite on :3000, then open the printed URL.
// Serves just enough Connect JSON, the run SSE stream, and the terminal WebSocket from
// docs/interactive-sessions.md. The scenario: a Forge-style interview and an approval gate on
// `plan`, plus a 3-cycle verify loop that hits its cap and escalates. Answering the
// escalation moves the run to human review.
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
};

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

server.listen(8001, "127.0.0.1", async () => {
  const vite = await createVite({ root: new URL("..", import.meta.url).pathname });
  await vite.listen();
  console.log(`[mock] API on http://127.0.0.1:8001 · open http://127.0.0.1:3000/projects/${projectId}/runs/${runId}`);
});
