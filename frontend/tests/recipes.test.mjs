import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import { test } from "node:test";
import { createServer } from "vite";

test("recipe mutations send CSRF, launch sends the library version, and the form helpers keep references", async () => {
  const previousWindow = globalThis.window;
  const previousFetch = globalThis.fetch;
  globalThis.window = { location: { origin: "https://blaxsmith.test" } };
  const server = await createServer({ server: { middlewareMode: true }, appType: "custom" });
  const requests = [];
  globalThis.fetch = async (input, init) => {
    if (String(input).endsWith("/GetCsrf")) {
      return new Response(JSON.stringify({ token: "C".repeat(43) }), { status: 200, headers: { "content-type": "application/json" } });
    }
    requests.push({ url: String(input).split("/api/")[1], body: JSON.parse(new TextDecoder().decode(init.body)), csrf: new Headers(init.headers).get("X-Blaxsmith-CSRF") });
    return new Response("{}", { status: 200, headers: { "content-type": "application/json" } });
  };
  try {
    const recipes = await server.ssrLoadModule("/src/recipes.ts");
    const { launchRun } = await server.ssrLoadModule("/src/workflow.ts");
    await recipes.createRecipe("", "Fast", "", "{}", "");
    await recipes.createRecipeVersion("r1", "{}", "", true);
    await recipes.setCurrentRecipeVersion("r1", "v2");
    await recipes.cloneRecipe("v2", "p1", "Copy", "");
    await recipes.validateRecipe("{}", "");
    await launchRun("p1", "key", "", "spec.md", "t.md", ".", "v2");
    assert.deepEqual(requests.map((r) => [r.url, r.csrf]), [
      ["blaxsmith.api.v1.RecipeService/CreateRecipe", "C".repeat(43)],
      ["blaxsmith.api.v1.RecipeService/CreateRecipeVersion", "C".repeat(43)],
      ["blaxsmith.api.v1.RecipeService/SetCurrentRecipeVersion", "C".repeat(43)],
      ["blaxsmith.api.v1.RecipeService/CloneRecipe", "C".repeat(43)],
      ["blaxsmith.api.v1.RecipeService/ValidateRecipe", null],
      ["blaxsmith.api.v1.WorkflowService/LaunchRun", "C".repeat(43)],
    ]);
    assert.equal(requests[5].body.recipeVersionId, "v2");
    assert.equal(requests[5].body.recipePath, undefined);

    const guild = recipes.parseRecipe(await readFile(new URL("../../examples/guild/recipe.json", import.meta.url), "utf8"));
    assert.ok(guild);
    const rows = recipes.stageRows(guild, ["plan", "implement", "verify", "review", "architect-review", "human-review"]);
    assert.deepEqual(rows.map((r) => r.id), ["plan", "implement", "verify", "review", "architect-review", "human-review"]);
    assert.equal(rows[2].loop, "implement until pass · ≤3");
    assert.equal(rows[1].harness, "codex");
    const renamed = recipes.renameStage(guild, "implement", "build");
    assert.deepEqual(renamed.stages.find((s) => s.id === "verify").loop.with, "build");
    assert.deepEqual(renamed.stages.find((s) => s.id === "review").depends_on, ["build"]);
    const profiles = recipes.renameProfile(guild, "reviewer", "critic");
    assert.ok(profiles.profiles.critic && !profiles.profiles.reviewer);
    assert.equal(profiles.stages.find((s) => s.id === "review").profile, "critic");
    assert.equal(recipes.parseRecipe("{ nope"), null);
    assert.deepEqual(recipes.parseRecipe(recipes.formatRecipe(guild)), guild);
    assert.deepEqual(await recipes.connectionModels.listConnectionModels({ models: ["opus"] }), ["opus"]);
    assert.deepEqual(["owner", "admin", "member", "viewer"].map((role) => recipes.mayEditRecipes({ role })), [true, true, false, false]);
  } finally {
    await server.close();
    globalThis.window = previousWindow;
    globalThis.fetch = previousFetch;
  }
});
