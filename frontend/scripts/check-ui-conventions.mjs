import { readdir, readFile } from "node:fs/promises";
import { join, relative, resolve } from "node:path";

const src = resolve(import.meta.dirname, "../src");
const css = join(src, "styles.css");
const table = join(src, "data-table.tsx");
const main = join(src, "main.tsx");
const failures = [];

async function visit(dir) {
  for (const entry of await readdir(dir, { withFileTypes: true })) {
    const path = join(dir, entry.name);
    if (entry.isDirectory()) { await visit(path); continue; }
    if (entry.name.endsWith(".css") && path !== css) failures.push(`${relative(src, path)}: use the global stylesheet`);
    if (!/\.[jt]sx?$/.test(entry.name)) continue;
    const source = await readFile(path, "utf8");
    if (path !== main && /import\s+["'][^"']+\.css["']/.test(source)) failures.push(`${relative(src, path)}: stylesheet import outside main`);
    if (path !== table && /<(?:table|thead|tbody|tr|th|td)(?=[\s/>])/.test(source)) failures.push(`${relative(src, path)}: use DataTable`);
    if (/\bstyle\s*=\s*\{/.test(source)) failures.push(`${relative(src, path)}: use global CSS classes`);
    if (/\b(?:window\.)?(?:alert|confirm|prompt|open)\s*\(/.test(source)) failures.push(`${relative(src, path)}: use routed UI or app confirmation`);
  }
}

await visit(src);
if (!(await readFile(main, "utf8")).includes('import "./styles.css"')) failures.push("main.tsx: global stylesheet missing");
if (failures.length) { console.error(failures.join("\n")); process.exitCode = 1; }
