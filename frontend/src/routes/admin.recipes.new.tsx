import { createFileRoute } from "@tanstack/react-router";
import { RecipeEditorPage } from "../recipe-pages";

export const Route = createFileRoute("/admin/recipes/new")({
  validateSearch: (search: Record<string, unknown>): { from?: string } => (typeof search.from === "string" ? { from: search.from } : {}),
  component: NewRecipe,
});

function NewRecipe() {
  const { from } = Route.useSearch();
  return <RecipeEditorPage key={from || "new"} from={from} />;
}
