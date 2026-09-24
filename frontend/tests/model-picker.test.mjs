import assert from "node:assert/strict";
import { after, before, test } from "node:test";
import { createServer } from "vite";

let server;
let m;
before(async () => {
  server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  m = await server.ssrLoadModule("/src/model-picker.ts");
});
after(() => server.close());

const model = (instanceId, slug, extra = {}) => ({ instanceId, slug, name: slug, connectionLabel: instanceId === "a" ? "Anthropic prod" : "OpenAI team",
  provider: instanceId === "a" ? "anthropic" : "openai", recommended: false, legacy: false, isDefault: false, badge: "", ...extra });
const models = [
  model("a", "claude-sonnet-5"),
  model("a", "claude-opus-5", { isDefault: true }),
  model("a", "claude-opus-4-6", { legacy: true }),
  model("o", "gpt-6-luna", { recommended: true }),
  model("o", "gpt-6-sol"),
];
const labels = (groups) => groups.map((g) => `${g.label}: ${g.models.map((x) => x.slug).join(",")}`);

test("recommended first, then favourites, then connections with the default leading; legacy hidden", () => {
  const groups = m.pickerGroups(models, { favorites: new Set(["o:gpt-6-sol"]), showLegacy: false, query: "", connectionOrder: ["a", "o"] });
  assert.deepEqual(labels(groups), [
    "Recommended: gpt-6-luna",
    "Favourites: gpt-6-sol",
    "Anthropic prod: claude-opus-5,claude-sonnet-5",
  ]);
});

test("legacy shows with the toggle or when selected", () => {
  const shown = m.pickerGroups(models, { favorites: new Set(), showLegacy: true, query: "", connectionOrder: ["a", "o"] });
  assert.ok(labels(shown).some((l) => l.includes("claude-opus-4-6")));
  const selected = m.pickerGroups(models, { favorites: new Set(), showLegacy: false, query: "", selected: "a:claude-opus-4-6", connectionOrder: ["a", "o"] });
  assert.ok(labels(selected).some((l) => l.includes("claude-opus-4-6")));
});

test("search ranks prefix matches first and matches connection labels", () => {
  const byName = m.pickerGroups(models, { favorites: new Set(), showLegacy: false, query: "opus", connectionOrder: ["a", "o"] });
  assert.deepEqual(labels(byName), ["Matches: claude-opus-5"]);
  const byConnection = m.pickerGroups(models, { favorites: new Set(), showLegacy: false, query: "openai team", connectionOrder: ["a", "o"] });
  assert.deepEqual(byConnection[0].models.map((x) => x.slug), ["gpt-6-luna", "gpt-6-sol"]);
  assert.deepEqual(m.pickerGroups(models, { favorites: new Set(), showLegacy: false, query: "zzz", connectionOrder: ["a", "o"] }), []);
});

test("t3code ordering helpers: favourites grouped, explicit order, stable otherwise", () => {
  const sorted = m.sortModelsForProviderInstance([{ slug: "a" }, { slug: "b" }, { slug: "c" }], { modelOrder: ["c"], favoriteModels: new Set(["b"]), groupFavorites: true });
  assert.deepEqual(sorted.map((x) => x.slug), ["b", "c", "a"]);
  const items = m.sortProviderModelItems([{ instanceId: "x", slug: "1" }, { instanceId: "y", slug: "2" }], { instanceOrder: ["y", "x"] });
  assert.deepEqual(items.map((x) => x.instanceId), ["y", "x"]);
  const fav = m.scoreModelPickerSearch({ driverKind: "openai", providerDisplayName: "Team", name: "gpt-6-sol", isFavorite: true }, "gpt");
  const plain = m.scoreModelPickerSearch({ driverKind: "openai", providerDisplayName: "Team", name: "gpt-6-sol" }, "gpt");
  assert.ok(fav < plain);
  assert.equal(m.scoreQueryMatch({ value: "claude-opus-5", query: "claude-opus-5", exactBase: 0 }), 0);
  assert.notEqual(m.scoreSubsequenceMatch("claude-opus-5", "cop"), null);
});
