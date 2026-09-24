import { createClient } from "@connectrpc/connect";
import { browserTransport, csrfToken } from "./auth";
import type { SessionIdentity } from "./gen/blaxsmith/api/v1/auth_pb";
import { RecipeService } from "./gen/blaxsmith/api/v1/recipes_pb";

const client = createClient(RecipeService, browserTransport);
const csrf = async () => ({ headers: { "X-Blaxsmith-CSRF": await csrfToken() } });

// Scope: an organization recipe has no project; a project recipe has one.
export type RecipeScope = { projectId?: string };

export const recipesKey = (org: string, projectId = "") => ["recipes", org, projectId] as const;
export const recipeKey = (org: string, recipeId: string) => ["recipe", org, recipeId] as const;
export const recipeVersionKey = (org: string, versionId: string) => ["recipe-version", org, versionId] as const;
export const recipeOptionsKey = (org: string) => ["recipe-editor-options", org] as const;
export const recipeFilesKey = (org: string, projectId: string) => ["recipe-files", org, projectId] as const;

// The server rechecks every edit; this only hides controls. Project admins
// are organization owners/admins (there are no project-level roles).
export const mayEditRecipes = (session?: SessionIdentity | null) => session?.role === "owner" || session?.role === "admin";

export const listRecipes = (projectId = "", signal?: AbortSignal) => client.listRecipes({ projectId }, { signal });
export const getRecipe = (recipeId: string, signal?: AbortSignal) => client.getRecipe({ recipeId }, { signal });
export const getRecipeVersion = (versionId: string, signal?: AbortSignal) => client.getRecipeVersion({ versionId }, { signal });
export const validateRecipe = (recipeJson: string, frozenPath: string, signal?: AbortSignal) => client.validateRecipe({ recipeJson, frozenPath }, { signal });
export const getRecipeEditorOptions = (signal?: AbortSignal) => client.getRecipeEditorOptions({}, { signal });
export const listProjectRecipeFiles = (projectId: string, signal?: AbortSignal) => client.listProjectRecipeFiles({ projectId }, { signal });

export async function createRecipe(projectId: string, name: string, description: string, recipeJson: string, frozenPath: string) {
  return client.createRecipe({ projectId, name, description, recipeJson, frozenPath }, await csrf());
}
export async function createRecipeVersion(recipeId: string, recipeJson: string, frozenPath: string, makeCurrent: boolean) {
  return client.createRecipeVersion({ recipeId, recipeJson, frozenPath, makeCurrent }, await csrf());
}
export async function setCurrentRecipeVersion(recipeId: string, versionId: string) {
  return client.setCurrentRecipeVersion({ recipeId, versionId }, await csrf());
}
export async function cloneRecipe(sourceVersionId: string, projectId: string, name: string, description: string) {
  return client.cloneRecipe({ sourceVersionId, projectId, name, description }, await csrf());
}

// Recipe JSON shape, as written by the editor. Unknown fields are preserved
// by editing a parsed copy of the document.
export type RecipeProfile = { harness: string; model: string; effort: string; instructions?: string[]; skills?: string[] };
export type RecipeLoop = { with: string; until: string; max_cycles: number };
export type RecipeStage = { id: string; kind: string; profile?: string; depends_on?: string[]; prompt?: string; loop?: RecipeLoop };
export type RecipeDocument = {
  schema_version: string; name: string; profiles: Record<string, RecipeProfile>; stages: RecipeStage[];
  required_checks: string[]; limits: { max_correction_cycles: number; timeout_seconds: number; max_runtime_seconds?: number };
};

export const emptyRecipe = (): RecipeDocument => ({
  schema_version: "blaxsmith.recipe/v1alpha1", name: "new-recipe",
  profiles: { architect: { harness: "claude-code", model: "opus", effort: "high" } },
  stages: [
    { id: "plan", kind: "plan", profile: "architect", prompt: "prompts/plan.md" },
    { id: "human-review", kind: "human_review", depends_on: ["plan"] },
  ],
  required_checks: ["project-tests"], limits: { max_correction_cycles: 3, timeout_seconds: 1800, max_runtime_seconds: 0 },
});

export function parseRecipe(json: string): RecipeDocument | null {
  try {
    const value = JSON.parse(json) as unknown;
    if (!value || typeof value !== "object" || Array.isArray(value)) return null;
    const doc = value as Partial<RecipeDocument>;
    if (typeof doc.profiles !== "object" || !Array.isArray(doc.stages)) return null;
    return doc as RecipeDocument;
  } catch {
    return null;
  }
}

export const formatRecipe = (doc: RecipeDocument) => `${JSON.stringify(doc, null, 2)}\n`;

// Stage rows for the read-only DAG view, in server topological order when known.
export type StageRow = { id: string; kind: string; profile: string; harness: string; model: string; effort: string; dependsOn: string[]; loop: string };
export function stageRows(doc: RecipeDocument | null, order: string[] = []): StageRow[] {
  if (!doc) return [];
  const rows = doc.stages.map((stage) => {
    const profile = stage.profile ? doc.profiles?.[stage.profile] : undefined;
    return { id: stage.id, kind: stage.kind, profile: stage.profile || "", harness: profile?.harness || "", model: profile?.model || "",
      effort: profile?.effort || "", dependsOn: stage.depends_on || [],
      loop: stage.loop ? `${stage.loop.with} until ${stage.loop.until} · ≤${stage.loop.max_cycles}` : "" };
  });
  if (!order.length) return rows;
  const rank = new Map(order.map((id, index) => [id, index]));
  return [...rows].sort((a, b) => (rank.get(a.id) ?? 1e9) - (rank.get(b.id) ?? 1e9));
}

// Rename a profile or stage everywhere it is referenced.
export function renameStage(doc: RecipeDocument, from: string, to: string): RecipeDocument {
  return { ...doc, stages: doc.stages.map((stage) => ({
    ...stage, id: stage.id === from ? to : stage.id,
    ...(stage.depends_on ? { depends_on: stage.depends_on.map((dep) => dep === from ? to : dep) } : {}),
    ...(stage.loop ? { loop: { ...stage.loop, with: stage.loop.with === from ? to : stage.loop.with } } : {}),
  })) };
}
export function renameProfile(doc: RecipeDocument, from: string, to: string): RecipeDocument {
  const profiles = Object.fromEntries(Object.entries(doc.profiles).map(([key, value]) => [key === from ? to : key, value]));
  return { ...doc, profiles, stages: doc.stages.map((stage) => stage.profile === from ? { ...stage, profile: to } : stage) };
}
