// Credential-free probe of Claude Code's non-bare subscription argv
// (tooladapter.claudeSubscriptionArgs). A loopback mock stands in for the
// Anthropic API; the OAuth token is fake. Ambient traps are planted in the
// fresh config dir and in a parent of the work dir; none may reach the model.
// Usage: CLAUDE_BIN=/path/to/claude node deploy/runtime-proof/claude-subscription-probe.mjs
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdir, mkdtemp, rm, stat, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const binary = process.env.CLAUDE_BIN || "claude";
const skillMarker = "BLAXSMITH_SCOPED_SKILL_2f7c";
const traps = ["AMBIENT_USER_MEMORY_91a0", "AMBIENT_PARENT_MEMORY_5d11", "AMBIENT_USER_SKILL_77be", "AMBIENT_MCP_SERVER_c3d9", "AMBIENT_PARENT_SKILL_0e42"];
let seen;

const server = createServer(async (req, res) => {
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  const text = Buffer.concat(chunks).toString("utf8");
  if (!req.url?.includes("/messages")) return res.writeHead(404).end();
  let body = {};
  try { body = JSON.parse(text); } catch {}
  seen.requests++;
  seen.authorization ||= req.headers.authorization === "Bearer fake-oauth-access-token";
  seen.apiKeyHeader ||= Boolean(req.headers["x-api-key"]);
  seen.skill ||= text.includes(skillMarker);
  for (const trap of traps) if (text.includes(trap)) seen.leaks.add(trap);
  for (const tool of body.tools ?? []) seen.tools.add(tool.name);
  const message = { id: "msg_probe", type: "message", role: "assistant", model: body.model, content: [{ type: "text", text: "PROBE_OK" }], stop_reason: "end_turn", stop_sequence: null, usage: { input_tokens: 1, output_tokens: 1 } };
  if (body.stream !== true) return res.writeHead(200, { "content-type": "application/json" }).end(JSON.stringify(message));
  res.writeHead(200, { "content-type": "text/event-stream" });
  res.end([
    `event: message_start\ndata: ${JSON.stringify({ type: "message_start", message: { ...message, content: [], stop_reason: null } })}`,
    `event: content_block_start\ndata: ${JSON.stringify({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } })}`,
    `event: content_block_delta\ndata: ${JSON.stringify({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "PROBE_OK" } })}`,
    `event: content_block_stop\ndata: ${JSON.stringify({ type: "content_block_stop", index: 0 })}`,
    `event: message_delta\ndata: ${JSON.stringify({ type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 1 } })}`,
    `event: message_stop\ndata: ${JSON.stringify({ type: "message_stop" })}`, "",
  ].join("\n\n"));
});

function run(args, cwd, env) {
  return new Promise((resolve, reject) => {
    const child = spawn(binary, args, { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "", stderr = "";
    const timer = setTimeout(() => child.kill("SIGKILL"), 60_000);
    child.stdout.on("data", (c) => { stdout += c; });
    child.stderr.on("data", (c) => { stderr += c; });
    child.on("error", reject);
    child.on("close", (code) => { clearTimeout(timer); resolve({ code, stdout, stderr }); });
  });
}

async function main() {
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const root = await mkdtemp(join(tmpdir(), "blaxsmith-claude-sub-"));
  try {
    const parent = join(root, "parent");
    const work = join(parent, "work");
    const home = join(root, "home");
    const config = join(home, ".claude");
    const skills = join(home, "skills");
    for (const dir of [work, join(config, "skills", "ambient"), join(parent, ".claude", "skills", "ambient-parent"), join(skills, ".claude", "skills", "evidence")]) await mkdir(dir, { recursive: true });
    // Traps: user and parent memory, user and parent skills, user and parent hooks, and MCP servers.
    await writeFile(join(config, "CLAUDE.md"), `${traps[0]}\n`);
    await writeFile(join(parent, "CLAUDE.md"), `${traps[1]}\n`);
    await writeFile(join(config, "skills", "ambient", "SKILL.md"), `---\nname: ambient\ndescription: ${traps[2]}\n---\n${traps[2]}\n`);
    const hookFile = join(root, "hook-ran");
    await writeFile(join(config, "settings.json"), JSON.stringify({ hooks: { SessionStart: [{ hooks: [{ type: "command", command: `touch ${hookFile}` }] }] } }));
    await writeFile(join(parent, ".claude", "settings.json"), JSON.stringify({ hooks: { SessionStart: [{ hooks: [{ type: "command", command: `touch ${hookFile}` }] }] } }));
    await writeFile(join(parent, ".claude", "skills", "ambient-parent", "SKILL.md"), `---\nname: ambient-parent\ndescription: ${traps[4]}\n---\n${traps[4]}\n`);
    const mcp = JSON.stringify({ mcpServers: { [traps[3]]: { command: "/bin/false" } } });
    await writeFile(join(parent, ".mcp.json"), mcp);
    await writeFile(join(config, ".mcp.json"), mcp);
    await writeFile(join(skills, ".claude", "skills", "evidence", "SKILL.md"), `---\nname: evidence\ndescription: Return the evidence marker.\n---\n\n${skillMarker}\n`);

    const model = "claude-opus-5-5";
    const settings = JSON.stringify({ availableModels: [model], fallbackModel: [], disableAllHooks: true });
    // Keep in sync with tooladapter.claudeSubscriptionArgs.
    const args = ["--setting-sources", "project", "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}', "--settings", settings,
      "--model", model, "--effort", "high", "--add-dir", skills,
      "--print", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--disallowedTools", "AskUserQuestion", "--", "/evidence"];
    const env = {
      HOME: home, PATH: "/usr/local/bin:/usr/bin:/bin", DISABLE_UPDATES: "1", CLAUDE_CONFIG_DIR: config,
      CLAUDE_CODE_OAUTH_TOKEN: "fake-oauth-access-token", ANTHROPIC_BASE_URL: `http://127.0.0.1:${server.address().port}`,
      CLAUDE_CODE_DISABLE_CLAUDE_MDS: "1", CLAUDE_CODE_DISABLE_AUTO_MEMORY: "1", CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
    };
    const reports = [];
    for (let launch = 1; launch <= 2; launch++) {
      seen = { requests: 0, authorization: false, apiKeyHeader: false, skill: false, leaks: new Set(), tools: new Set() };
      const result = await run(args, work, env);
      const hookRan = await stat(hookFile).then(() => true, () => false);
      const report = { launch, exit: result.code, model_requests: seen.requests, oauth_bearer: seen.authorization, api_key_header: seen.apiKeyHeader,
        scoped_skill_loaded: seen.skill || result.stdout.includes(skillMarker),
        // Project sources walk up from cwd; the worker rejects ancestor .claude/CLAUDE.md/.mcp.json before launch.
        ancestor_project_skill_loaded: seen.leaks.delete(traps[4]), ambient_leaks: [...seen.leaks], hook_ran: hookRan,
        ask_user_question_offered: seen.tools.has("AskUserQuestion"), mcp_tools: [...seen.tools].filter((name) => name.startsWith("mcp__")) };
      reports.push(report);
      if (result.code !== 0 || !report.oauth_bearer || report.api_key_header || !report.scoped_skill_loaded || report.ambient_leaks.length ||
        report.hook_ran || report.ask_user_question_offered || report.mcp_tools.length) {
        throw new Error(`isolation failed: ${JSON.stringify(report)} stderr=${result.stderr.slice(-1500)} stdout=${result.stdout.slice(-800)}`);
      }
    }
    const version = await run(["--version"], work, env);
    console.log(JSON.stringify({ schema_version: "blaxsmith.claude-subscription-probe/v1", claude_code: version.stdout.trim(),
      provider: "loopback mock; fake OAuth token", reports }, null, 2));
  } finally {
    await rm(root, { recursive: true, force: true });
  }
}

try { await main(); } catch (error) { console.error(error instanceof Error ? error.message : String(error)); process.exitCode = 1; } finally { server.close(); }
