// Verifies a runner variant's runtime layers against /opt/blaxsmith/runtimes.json
// and prints them with binary hashes. manifest.mjs includes the result.
import { createHash } from "node:crypto";
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { pathToFileURL } from "node:url";

const run = (binary, args) => execFileSync(binary, args, { encoding: "utf8", timeout: 20_000, env: { ...process.env, UV_PYTHON_DOWNLOADS: "never", UV_OFFLINE: "1" } }).trim();
const hash = (path) => createHash("sha256").update(readFileSync(path)).digest("hex");

export function runtimes(path = "/opt/blaxsmith/runtimes.json") {
  if (!existsSync(path)) return null;
  const pins = JSON.parse(readFileSync(path, "utf8"));
  const { uv, python, serena } = pins.runtimes;
  const uvBinary = "/usr/local/bin/uv";
  if (run(uvBinary, ["--version"]).split(" ")[1] !== uv.version) throw new Error("uv version changed");
  const pythonBinary = run(uvBinary, ["python", "find", "--managed-python", python.version]);
  const pythonVersion = run(pythonBinary, ["--version"]).replace(/^Python /, "");
  if (!pythonVersion.startsWith(`${python.version}.`)) throw new Error("Python version changed");
  if (!run(uvBinary, ["tool", "list"]).split("\n").includes(`${serena.package} v${serena.version}`)) throw new Error("Serena version changed");
  const serenaBinary = "/usr/local/bin/serena";
  return {
    variant: pins.variant,
    exclude_newer: pins.exclude_newer,
    layers: [
      { id: "uv", version: uv.version, binary: uvBinary, binary_sha256: hash(uvBinary) },
      { id: "python", version: pythonVersion, binary: pythonBinary, binary_sha256: hash(pythonBinary) },
      { id: "serena", version: serena.version, binary: serenaBinary, binary_sha256: hash(serenaBinary) },
    ],
  };
}

if (import.meta.url === pathToFileURL(process.argv[1]).href) {
  const result = runtimes();
  if (!result) throw new Error("no runtime pins in this image");
  process.stdout.write(JSON.stringify(result) + "\n");
}
