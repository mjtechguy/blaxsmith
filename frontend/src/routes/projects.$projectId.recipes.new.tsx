import { createFileRoute } from "@tanstack/react-router";
import { RecipeEditorPage } from "../recipe-pages";

export const Route = createFileRoute("/projects/$projectId/recipes/new")({
  validateSearch: (search: Record<string, unknown>): { from?: string } => (typeof search.from === "string" ? { from: search.from } : {}),
  component: NewRecipe,
});

function NewRecipe() {
  const { projectId } = Route.useParams();
  const { from } = Route.useSearch();
  return <RecipeEditorPage key={`${projectId}:${from || ""}`} projectId={projectId} from={from} />;
}
