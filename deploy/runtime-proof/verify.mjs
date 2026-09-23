import { execFileSync } from "node:child_process";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL(".", import.meta.url));
const installed = (name) => {
  try {
    return JSON.parse(readFileSync(join(root, "node_modules", name, "package.json"), "utf8"));
  } catch {
    return null;
  }
};
const lock = JSON.parse(readFileSync(join(root, "package-lock.json"), "utf8"));

const tools = [
  ["@openai/codex", "0.156.1", "@openai/codex-linux-x64", "0.156.1-linux-x64", "codex", "codex-cli 0.156.1"],
  ["@anthropic-ai/claude-code", "2.1.280", "@anthropic-ai/claude-code-linux-x64", "2.1.280", "claude", "2.1.280 (Claude Code)"],
  ["@opencode/cli", "2.0.14", "@opencode/cli-linux-x64", "2.0.14", "opencode", "opencode v2.0.14"],
];

if (process.platform !== "linux" || process.arch !== "x64") throw new Error("Linux/x64 required");
if (!process.report.getReport().header.glibcVersionRuntime) throw new Error("glibc required");
for (const [rootName, rootVersion, nativeName, nativeVersion, command, expected] of tools) {
  for (const [name, version] of [[rootName, rootVersion], [nativeName, nativeVersion]]) {
    const entry = lock.packages[`node_modules/${name}`];
    if (!entry?.integrity?.startsWith("sha512-") || entry.version !== version || installed(name)?.version !== version) {
      throw new Error(`${name}@${version} missing or mismatched`);
    }
  }
  const native = lock.packages[`node_modules/${nativeName}`];
  if (!native.optional || !native.os.includes("linux") || !native.cpu.includes("x64")) {
    throw new Error(`${nativeName} is not the selected Linux/x64 optional package`);
  }
  if (native.libc && !native.libc.includes("glibc")) throw new Error(`${nativeName} is not the glibc package`);
  if (process.argv.includes("--packages-only")) continue;
  const actual = execFileSync(join(root, "node_modules", ".bin", command), ["--version"], {
    encoding: "utf8", timeout: 20_000,
  }).trim();
  if (actual !== expected) throw new Error(`${command}: expected ${expected}, got ${actual}`);
  console.log(`${nativeName}@${nativeVersion}: ${actual}`);
}
