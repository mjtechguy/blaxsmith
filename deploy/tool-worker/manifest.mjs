import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const root = "/opt/blaxsmith";
const packages = JSON.parse(readFileSync(join(root, "package.json"), "utf8"));
execFileSync(process.execPath, [join(root, "verify.mjs")], { timeout: 60_000 });
const hash = (path) => createHash("sha256").update(readFileSync(path)).digest("hex");
const tools = [
  ["codex", "codex", "@openai/codex", (version) => `codex-cli ${version}`],
  ["claude-code", "claude", "@anthropic-ai/claude-code", (version) => `${version} (Claude Code)`],
  ["opencode", "opencode", "@opencode/cli", (version) => `opencode v${version}`],
].map(([harness, name, packageName, expected]) => {
  const binary = join(root, "node_modules", ".bin", name);
  const version = packages.dependencies[packageName];
  const actual = execFileSync(binary, ["--version"], { encoding: "utf8", timeout: 20_000 }).trim();
  if (actual !== expected(version)) throw new Error(`${name} version changed`);
  return { harness, binary, binary_sha256: hash(binary), version };
});

const git = execFileSync("/usr/bin/git", ["--version"], { encoding: "utf8", timeout: 20_000 }).trim();
if (!git.startsWith("git version ")) throw new Error("Git unavailable");
process.stdout.write(JSON.stringify({
  schema: "blaxsmith.tool-worker-image/v1alpha1",
  platform: "linux/amd64",
  ax_runner_sha256: hash("/usr/local/bin/ax-task-runner"),
  tool_worker_sha256: hash("/usr/local/bin/blaxsmith-tool-worker"),
  git_version: git,
  tools,
}) + "\n");
