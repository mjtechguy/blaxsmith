import { createFileRoute } from "@tanstack/react-router";
import { RecipeDetailPage } from "../recipe-pages";

export const Route = createFileRoute("/projects/$projectId/recipes/$recipeId/")({ component: Detail });

function Detail() {
  const { projectId, recipeId } = Route.useParams();
  return <RecipeDetailPage key={recipeId} projectId={projectId} recipeId={recipeId} />;
}
