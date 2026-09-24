import { createFileRoute } from "@tanstack/react-router";
import { RecipeEditorPage } from "../recipe-pages";

export const Route = createFileRoute("/projects/$projectId/recipes/$recipeId/versions/new")({
  validateSearch: (search: Record<string, unknown>): { from?: string } => (typeof search.from === "string" ? { from: search.from } : {}),
  component: NewVersion,
});

function NewVersion() {
  const { projectId, recipeId } = Route.useParams();
  const { from } = Route.useSearch();
  return <RecipeEditorPage key={`${recipeId}:${from || ""}`} projectId={projectId} recipeId={recipeId} from={from} />;
}
