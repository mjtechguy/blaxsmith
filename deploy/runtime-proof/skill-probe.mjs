import { createHash } from "node:crypto";
import { spawn } from "node:child_process";
import { createServer } from "node:http";
import { mkdir, mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";

const marker = "BLAXSMITH_NATIVE_SKILL_BODY_6cfd3b72";
const skill = `---\nname: evidence\ndescription: Return the unique evidence marker when asked to use this skill.\n---\n\n${marker}\n`;
const digest = createHash("sha256").update(skill).digest("hex");
const reports = [];
let current;

function reply(res, body, stream = false) {
  if (stream) {
    if (body.object === "chat.completion") {
      const choice = body.choices[0];
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      const emit = (delta, finish_reason = null) => res.write(`data: ${JSON.stringify({ id: body.id, object: "chat.completion.chunk", created: body.created, model: body.model, choices: [{ index: choice.index, delta, finish_reason }] })}\n\n`);
      emit({ role: "assistant" });
      if (choice.message.tool_calls?.length) emit({ tool_calls: choice.message.tool_calls });
      else emit({ content: choice.message.content ?? "" });
      emit({}, choice.finish_reason);
      res.end("data: [DONE]\n\n");
      return;
    }
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    res.end(`data: ${JSON.stringify(body)}\n\ndata: [DONE]\n\n`);
  } else {
    res.writeHead(200, { "content-type": "application/json" });
    res.end(JSON.stringify(body));
  }
}

function chatCompletion(model, message, toolCall) {
  const body = {
    id: `chatcmpl-probe-${current.turns}`,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, message, finish_reason: toolCall ? "tool_calls" : "stop" }],
    usage: { prompt_tokens: 1, completion_tokens: 1, total_tokens: 2 },
  };
  if (toolCall) body.choices[0].message.tool_calls = [toolCall];
  return body;
}

function webSocketText(value) {
  const payload = Buffer.from(JSON.stringify(value));
  if (payload.length < 126) return Buffer.concat([Buffer.from([0x81, payload.length]), payload]);
  const header = Buffer.alloc(4);
  header[0] = 0x81;
  header[1] = 126;
  header.writeUInt16BE(payload.length, 2);
  return Buffer.concat([header, payload]);
}

const server = createServer(async (req, res) => {
  if (req.method === "GET" && req.url?.endsWith("/models")) {
    reply(res, { data: [{ id: "probe", object: "model", created: 1, owned_by: "blaxsmith" }] });
    return;
  }
  const chunks = [];
  for await (const chunk of req) chunks.push(chunk);
  let body;
  try {
    body = JSON.parse(Buffer.concat(chunks).toString("utf8"));
  } catch {
    res.writeHead(400).end();
    return;
  }
  current.turns++;
  if (current.turns > 10) {
    res.writeHead(429).end();
    return;
  }
  const serialized = JSON.stringify(body);
  current.requests.push({ turn: current.turns, path: req.url, prompt: serialized.includes("Use the evidence skill"), marker: serialized.includes(marker), roles: (body.messages ?? []).map((message) => message.role), toolNames: (body.tools ?? []).map((tool) => tool.function?.name ?? tool.name ?? tool.tool).filter(Boolean) });
  current.skillDiscovered ||= (body.tools ?? []).some((tool) =>
    (tool.function?.name ?? tool.name ?? tool.tool) === "skill",
  );
  current.skillLoaded ||= serialized.includes(marker);

  if (current.harness === "opencode") {
    if (!current.skillRequested && serialized.includes("Use the evidence skill")) {
      current.skillRequested = true;
      const tool = (body.tools ?? []).find((item) => (item.function?.name ?? item.name ?? item.tool) === "skill");
      const call = tool && {
        id: "call-load-evidence",
        type: "function",
        function: { name: tool.function?.name ?? tool.name ?? "skill", arguments: JSON.stringify({ id: "evidence" }) },
      };
      reply(res, chatCompletion(body.model || "probe", { role: "assistant", content: "", tool_calls: call ? [call] : [] }, call), body.stream === true);
      return;
    }
  }

  if (req.url?.includes("/messages")) {
    const model = body.model || "claude-opus-5-5";
    const message = { id: "msg_probe", type: "message", role: "assistant", model, content: [{ type: "text", text: "PROBE_OK" }], stop_reason: "end_turn", stop_sequence: null, usage: { input_tokens: 1, output_tokens: 1 } };
    if (body.stream === true) {
      res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
      res.end([
        `event: message_start\ndata: ${JSON.stringify({ type: "message_start", message: { ...message, content: [], stop_reason: null } })}`,
        `event: content_block_start\ndata: ${JSON.stringify({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } })}`,
        `event: content_block_delta\ndata: ${JSON.stringify({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "PROBE_OK" } })}`,
        `event: content_block_stop\ndata: ${JSON.stringify({ type: "content_block_stop", index: 0 })}`,
        `event: message_delta\ndata: ${JSON.stringify({ type: "message_delta", delta: { stop_reason: "end_turn", stop_sequence: null }, usage: { output_tokens: 1 } })}`,
        `event: message_stop\ndata: ${JSON.stringify({ type: "message_stop" })}`,
        "",
      ].join("\n\n"));
    } else reply(res, message);
    return;
  }

  if (req.url?.includes("/responses")) {
    const response = { id: "resp_probe", object: "response", created_at: Math.floor(Date.now() / 1000), status: "completed", model: body.model || "gpt-6-luna", output: [{ id: "msg_probe", type: "message", status: "completed", role: "assistant", content: [{ type: "output_text", text: "PROBE_OK", annotations: [] }] }], usage: { input_tokens: 1, output_tokens: 1, total_tokens: 2 } };
    res.writeHead(200, { "content-type": "text/event-stream", "cache-control": "no-cache" });
    res.end(`event: response.output_text.delta\ndata: ${JSON.stringify({ type: "response.output_text.delta", delta: "PROBE_OK" })}\n\nevent: response.completed\ndata: ${JSON.stringify({ type: "response.completed", response })}\n\n`);
    return;
  }

  if (req.url?.includes("/chat/completions")) {
    reply(res, chatCompletion(body.model || "probe", { role: "assistant", content: "PROBE_OK" }), body.stream === true);
    return;
  }
  res.writeHead(404).end();
});

server.on("upgrade", (req, socket) => {
  const key = req.headers["sec-websocket-key"];
  if (!key || !req.url?.startsWith("/v1/responses")) return socket.destroy();
  const accept = createHash("sha1").update(`${key}258EAFA5-E914-47DA-95CA-C5AB0DC85B11`).digest("base64");
  socket.write(`HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ${accept}\r\n\r\n`);
  let buffered = Buffer.alloc(0);
  socket.on("data", (chunk) => {
    buffered = Buffer.concat([buffered, chunk]);
    while (buffered.length >= 2) {
      const opcode = buffered[0] & 0x0f;
      let size = buffered[1] & 0x7f;
      let offset = 2;
      if (size === 126) {
        if (buffered.length < 4) return;
        size = buffered.readUInt16BE(2);
        offset = 4;
      } else if (size === 127) {
        if (buffered.length < 10) return;
        const large = buffered.readBigUInt64BE(2);
        if (large > 1_000_000n) return socket.destroy();
        size = Number(large);
        offset = 10;
      }
      const masked = (buffered[1] & 0x80) !== 0;
      if (buffered.length < offset + (masked ? 4 : 0) + size) return;
      const mask = masked ? buffered.subarray(offset, offset + 4) : undefined;
      offset += masked ? 4 : 0;
      const payload = Buffer.from(buffered.subarray(offset, offset + size));
      buffered = buffered.subarray(offset + size);
      if (mask) for (let i = 0; i < payload.length; i++) payload[i] ^= mask[i % 4];
      if (opcode === 8) return socket.end();
      if (opcode !== 1) continue;
      const body = JSON.parse(payload.toString("utf8"));
      current.turns++;
      current.skillLoaded ||= JSON.stringify(body).includes(marker);
      const responseID = `resp-${current.turns}`;
      const usage = { input_tokens: 1, input_tokens_details: null, output_tokens: 1, output_tokens_details: null, total_tokens: 2 };
      socket.write(webSocketText({ type: "response.created", response: { id: responseID } }));
      socket.write(webSocketText({ type: "response.output_item.done", item: { type: "message", role: "assistant", id: "msg-probe", content: [{ type: "output_text", text: "PROBE_OK" }] } }));
      socket.write(webSocketText({ type: "response.completed", response: { id: responseID, usage } }));
    }
  });
  socket.on("error", () => {});
});

async function run(binary, args, cwd, env) {
  return new Promise((resolve, reject) => {
    const child = spawn(binary, args, { cwd, env, stdio: ["ignore", "pipe", "pipe"] });
    let stdout = "";
    let stderr = "";
    const timer = setTimeout(() => child.kill("SIGKILL"), 45_000);
    child.stdout.on("data", (chunk) => { stdout += chunk; });
    child.stderr.on("data", (chunk) => { stderr += chunk; });
    child.on("error", reject);
    child.on("close", (code) => {
      clearTimeout(timer);
      resolve({ code, stdout, stderr });
    });
  });
}

async function main() {
  await new Promise((resolve) => server.listen(0, "127.0.0.1", resolve));
  const baseURL = `http://127.0.0.1:${server.address().port}`;
  for (const harness of ["codex", "claude", "opencode"]) {
    for (let restart = 1; restart <= 2; restart++) {
      const home = await mkdtemp(join(tmpdir(), "blaxsmith-native-skill-"));
      const work = join(home, "work");
      await mkdir(work, { recursive: true });
      await mkdir(join(home, ".codex"), { recursive: true });
      await mkdir(join(home, ".claude-config"), { recursive: true });
      let skillDir;
      if (harness === "codex") skillDir = join(home, ".agents", "skills", "evidence");
      if (harness === "claude") skillDir = join(home, "skill-source", ".claude", "skills", "evidence");
      if (harness === "opencode") skillDir = join(home, ".config", "opencode", "skills", "evidence");
      await mkdir(skillDir, { recursive: true });
      await writeFile(join(skillDir, "SKILL.md"), skill, { mode: 0o400 });

      const env = {
        HOME: home,
        PATH: "/opt/blaxsmith/node_modules/.bin:/usr/local/bin:/usr/bin:/bin",
        NO_COLOR: "1",
        CI: "1",
        DISABLE_UPDATES: "1",
        DISABLE_AUTOUPDATER: "1",
        CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: "1",
        CODEX_HOME: join(home, ".codex"),
        CLAUDE_CONFIG_DIR: join(home, ".claude-config"),
        XDG_CONFIG_HOME: join(home, ".config"),
        HTTP_PROXY: "http://127.0.0.1:9",
        HTTPS_PROXY: "http://127.0.0.1:9",
        ALL_PROXY: "http://127.0.0.1:9",
        NO_PROXY: "127.0.0.1,localhost",
      };
      current = { harness, restart, turns: 0, skillDiscovered: false, skillLoaded: false, skillRequested: false, requests: [] };
      let binary;
      let args;
      if (harness === "codex") {
        binary = "/opt/blaxsmith/node_modules/.bin/codex";
        env.OPENAI_API_KEY = "local-probe-only";
        args = ["exec", "--json", "--ephemeral", "--ignore-user-config", "--disable", "multi_agent", "--disable", "apps", "--disable", "plugins", "--sandbox", "read-only", "--model", "gpt-6-luna", "--config", 'approval_policy="never"', "--config", 'model_reasoning_effort="xhigh"', "--config", 'web_search="disabled"', "--config", "skills.bundled.enabled=false", "--config", `openai_base_url=${JSON.stringify(`${baseURL}/v1`)}`, "--skip-git-repo-check", "Use $evidence and return only its unique marker."];
      } else if (harness === "claude") {
        binary = "/opt/blaxsmith/node_modules/.bin/claude";
        env.ANTHROPIC_API_KEY = "local-probe-only";
        env.ANTHROPIC_BASE_URL = baseURL;
        args = ["/evidence", "--bare", "--print", "--output-format", "stream-json", "--verbose", "--permission-prompts", "none", "--no-session-persistence", "--settings", JSON.stringify({ availableModels: ["claude-opus-5-5"], fallbackModel: [] }), "--model", "claude-opus-5-5", "--effort", "high", "--add-dir", join(home, "skill-source")];
      } else {
        binary = "/opt/blaxsmith/node_modules/.bin/opencode";
        env.BLAXSMITH_PROBE_KEY = "local-probe-only";
        const skills = join(home, ".config", "opencode", "skills");
        const root = `${skills}/evidence/*`;
        const config = {
          $schema: "https://opencode.ai/config.json",
          update: "disable",
          providers: { blaxsmith: { name: "Local probe", env: ["BLAXSMITH_PROBE_KEY"], package: "@opencode/ai/providers/openai-compatible", settings: { baseURL: `${baseURL}/v1` }, models: { probe: { name: "Probe", modelID: "probe", variants: [{ id: "high", settings: { reasoningEffort: "high" } }], capabilities: { tools: true, input: ["text"], output: ["text"] }, limit: { context: 131072, output: 8192 } } } } },
          permissions: [
            { action: "*", resource: "*", effect: "allow" },
            { action: "subagent", resource: "*", effect: "deny" },
            { action: "question", resource: "*", effect: "deny" },
            { action: "webfetch", resource: "*", effect: "deny" },
            { action: "websearch", resource: "*", effect: "deny" },
            { action: "execute", resource: "*", effect: "deny" },
            { action: "skill", resource: "*", effect: "deny" },
            { action: "external_directory", resource: "*", effect: "deny" },
            { action: "external_directory", resource: root, effect: "allow" },
            { action: "read", resource: root, effect: "allow" },
            { action: "edit", resource: root, effect: "deny" },
            { action: "skill", resource: "evidence", effect: "allow" },
            { action: "read", resource: "*.env", effect: "deny" },
            { action: "read", resource: "*.env.*", effect: "deny" },
            { action: "read", resource: "*.env.example", effect: "allow" },
          ],
        };
        await writeFile(join(home, ".config", "opencode", "opencode.json"), JSON.stringify(config));
        args = ["run", "--standalone", "--format", "json", "--title", "Native skill probe", "--model", "blaxsmith/probe#high", "Use the evidence skill and return only its unique marker."];
      }
      try {
        const result = await run(binary, args, work, env);
        if (result.code !== 0 || !result.stdout.includes("PROBE_OK")) throw new Error(`${harness} exit ${result.code}: ${JSON.stringify(current)} ${result.stderr.slice(-2000)} ${result.stdout.slice(-1000)}`);
        const skillLoaded = current.skillLoaded || result.stdout.includes(marker);
        if (harness === "opencode" ? !current.skillDiscovered || !skillLoaded : !skillLoaded) {
          throw new Error(`${harness} did not expose and load the selected skill (discovered=${current.skillDiscovered}, loaded=${skillLoaded}, requests=${JSON.stringify(current.requests)}, stdout=${result.stdout.slice(-1500)}, stderr=${result.stderr.slice(-1000)})`);
        }
        reports.push({ harness, restart, skill_sha256: digest, native_skill_loaded: true, skill_tool_discovered: current.skillDiscovered, model_requests: current.turns });
      } finally {
        await rm(home, { recursive: true, force: true });
      }
    }
  }
  console.log(JSON.stringify({ schema_version: "blaxsmith.native-skill-probe/v1", tool_image_versions: { codex: "0.156.1", claude_code: "2.1.280", opencode: "2.0.14" }, provider: "loopback mock; fake key; container network disabled", skill_sha256: digest, reports }, null, 2));
}

try {
  await main();
} catch (error) {
  console.error(error instanceof Error ? error.message : String(error));
  process.exitCode = 1;
} finally {
  server.close();
}
