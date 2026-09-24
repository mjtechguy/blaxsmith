import { createFileRoute } from "@tanstack/react-router";
import { RecipeDetailPage } from "../recipe-pages";

export const Route = createFileRoute("/admin/recipes/$recipeId/")({ component: Detail });

function Detail() {
  const { recipeId } = Route.useParams();
  return <RecipeDetailPage key={recipeId} recipeId={recipeId} />;
}
