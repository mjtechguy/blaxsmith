import assert from "node:assert/strict";
import { test } from "node:test";
import { createServer } from "vite";

test("table view state round-trips through the URL and preferences stay per user", async () => {
  const saved = globalThis.localStorage;
  const store = new Map();
  globalThis.localStorage = { getItem: (k) => store.get(k) ?? null, setItem: (k, v) => store.set(k, String(v)) };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  try {
    const { decodeView, encodeView, withCriteria, readPrefs, writePrefs, prefsKey, columnVisibility, withVisibility } = await server.ssrLoadModule("/src/table-state.ts");
    const defaults = { sort: [{ id: "created", desc: true }], size: 20 };
    const view = { q: "invoice", sort: [{ id: "run", desc: false }], page: 3, size: 50, filters: { state: ["active", "failed"], kind: [] } };
    const encoded = encodeView(view, { tab: "stages", q: "old", f_state: "queued" }, defaults);
    // Unrelated params survive; empty filters and defaults drop out.
    assert.deepEqual(encoded, { tab: "stages", q: "invoice", sort: "run.asc", page: 3, size: 50, f_state: "active,failed" });
    // The router parses numbers; decoding tolerates both shapes.
    assert.deepEqual(decodeView(encoded, defaults), { ...view, filters: { state: ["active", "failed"] } });
    assert.deepEqual(decodeView(JSON.parse(JSON.stringify({ ...encoded, page: "3", size: "50" })), defaults), { ...view, filters: { state: ["active", "failed"] } });
    // Defaults are implicit in the URL.
    assert.deepEqual(encodeView(decodeView({}, defaults), {}, defaults), {});
    assert.deepEqual(decodeView({}, defaults), { q: "", sort: defaults.sort, page: 1, size: 20, filters: {} });
    // Malformed input falls back safely.
    assert.deepEqual(decodeView({ page: "-4", size: "7", sort: "bad id!.asc", q: 12345 }, defaults), { q: "12345", sort: defaults.sort, page: 1, size: 20, filters: {} });
    // New criteria return to page one.
    assert.equal(withCriteria(view, { q: "x" }).page, 1);

    writePrefs("user-a", "runs", { hidden: ["commit"], widths: { run: 240, bad: 5 }, density: "compact" });
    assert.deepEqual(readPrefs("user-a", "runs"), { hidden: ["commit"], shown: [], widths: { run: 240 }, density: "compact" });
    assert.deepEqual(readPrefs("user-b", "runs", "comfortable"), { hidden: [], shown: [], widths: {}, density: "comfortable" }, "another user never sees these settings");
    assert.deepEqual(readPrefs("user-b", "runs", "compact").density, "compact", "the account default applies to untouched tables");
    assert.equal(prefsKey("user-a", "runs"), "blaxsmith:table:v1:user-a:runs");

    // Default-hidden columns (Runs › State) stay off until the viewer turns them on;
    // existing prefs without "shown" still read, and the choice persists per user.
    const fresh = readPrefs("user-c", "runs");
    assert.deepEqual(columnVisibility(fresh, ["state"]), { state: false });
    const on = withVisibility(fresh, { ...columnVisibility(fresh, ["state"]), state: true }, ["state"]);
    assert.deepEqual([on.hidden, on.shown], [[], ["state"]]);
    assert.deepEqual(columnVisibility(on, ["state"]), {});
    const off = withVisibility(on, { commit: false, state: true }, ["state"]);
    assert.deepEqual([off.hidden, off.shown], [["commit"], ["state"]]);
    writePrefs("user-c", "runs", off);
    assert.deepEqual(columnVisibility(readPrefs("user-c", "runs"), ["state"]), { commit: false });
    assert.deepEqual(columnVisibility(readPrefs("user-d", "runs"), ["state"]), { state: false }, "another user keeps the default");
  } finally {
    await server.close();
    globalThis.localStorage = saved;
  }
});
